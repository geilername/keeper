"""Feature tests on small synthetic images generated here (no fixture files)."""

import cv2
import numpy as np
import pytest

from keeper_scorer import features


def scene(seed: int = 42, size: tuple[int, int] = (640, 480)) -> np.ndarray:
    """A blocky random grey scene: lots of hard edges, mean close to mid-grey."""
    rng = np.random.default_rng(seed)
    small = rng.integers(0, 256, (48, 64), dtype=np.uint8)
    return cv2.resize(small, size, interpolation=cv2.INTER_NEAREST)


def blurred(img: np.ndarray, sigma: float = 4.0) -> np.ndarray:
    return cv2.GaussianBlur(img, (0, 0), sigma)


def jpeg_roundtrip(img: np.ndarray, quality: int = 70) -> np.ndarray:
    ok, buf = cv2.imencode(".jpg", img, [cv2.IMWRITE_JPEG_QUALITY, quality])
    assert ok
    return cv2.imdecode(buf, cv2.IMREAD_GRAYSCALE)


# --- sharpness ---------------------------------------------------------------


def test_sharp_scores_higher_than_blurred():
    sharp = features.sharpness(scene())
    blur = features.sharpness(blurred(scene()))
    assert sharp["variance"] > 10 * blur["variance"]
    assert sharp["score"] > 0.9
    assert blur["score"] < 0.5


def test_more_blur_means_lower_score():
    scores = [features.sharpness(blurred(scene(), s))["score"] for s in (1, 2, 4, 8)]
    assert scores == sorted(scores, reverse=True)


def test_flat_image_has_zero_sharpness():
    assert features.sharpness(np.full((100, 100), 128, np.uint8))["score"] == 0.0


# --- exposure ----------------------------------------------------------------


def test_mid_grey_scene_is_well_exposed():
    e = features.exposure(scene())
    assert 0.4 < e["mean"] < 0.6
    assert e["score"] > 0.8


def test_dark_and_bright_score_lower_than_normal():
    normal = scene()
    dark = (normal * 0.15).astype(np.uint8)
    bright = np.clip(normal.astype(np.int16) + 180, 0, 255).astype(np.uint8)

    e_normal, e_dark, e_bright = (features.exposure(i) for i in (normal, dark, bright))
    assert e_dark["mean"] < 0.15
    assert e_bright["mean"] > 0.75
    assert e_dark["shadows_clipped"] > e_normal["shadows_clipped"]
    assert e_bright["highlights_clipped"] > 0.5
    assert e_normal["score"] > e_dark["score"]
    assert e_normal["score"] > e_bright["score"]
    assert e_bright["score"] == 0.0  # mostly clipped


def test_black_and_white_frames_score_zero():
    assert features.exposure(np.zeros((50, 50), np.uint8))["score"] == 0.0
    assert features.exposure(np.full((50, 50), 255, np.uint8))["score"] == 0.0


# --- dhash -------------------------------------------------------------------


def test_dhash_is_64_bit_hex_and_deterministic():
    h = features.dhash(scene())
    assert len(h) == 16
    int(h, 16)  # valid hex
    assert features.dhash(scene()) == h


@pytest.mark.parametrize(
    "variant",
    [
        lambda i: np.clip(i.astype(np.int16) + 15, 0, 255).astype(np.uint8),  # brighter
        lambda i: cv2.resize(i, (320, 240), interpolation=cv2.INTER_AREA),  # smaller
        jpeg_roundtrip,  # recompressed
        lambda i: blurred(i, 1.0),  # slight blur
    ],
    ids=["brighter", "resized", "jpeg", "slight-blur"],
)
def test_dhash_is_stable_under_small_changes(variant):
    base = scene()
    assert features.hamming(features.dhash(base), features.dhash(variant(base))) <= 6


def test_dhash_differs_for_different_scenes():
    assert features.hamming(features.dhash(scene(1)), features.dhash(scene(2))) >= 16


# --- decode ------------------------------------------------------------------


def test_decode_downscales_to_working_size():
    big = cv2.resize(scene(), (3000, 2000), interpolation=cv2.INTER_NEAREST)
    ok, buf = cv2.imencode(".jpg", big)
    assert ok
    gray = features.decode(buf.tobytes())
    assert gray.ndim == 2
    assert max(gray.shape) == features.WORK_LONG_EDGE
    assert gray.shape == (683, 1024)  # aspect ratio kept


def test_decode_keeps_small_images_as_they_are():
    ok, buf = cv2.imencode(".png", scene())
    assert features.decode(buf.tobytes()).shape == (480, 640)


@pytest.mark.parametrize("data", [b"", b"definitely not a jpeg", b"\xff\xd8\xff\xe0" + b"\x00" * 64])
def test_decode_rejects_corrupt_bytes(data):
    with pytest.raises(features.DecodeError):
        features.decode(data)


# --- eyes --------------------------------------------------------------------


def texture(seed: int) -> np.ndarray:
    """Smooth random texture at working size: the kind of input that fools Haar cascades."""
    rng = np.random.default_rng(seed)
    img = cv2.resize(rng.integers(0, 256, (60, 90), dtype=np.uint8), (1024, 683), interpolation=cv2.INTER_CUBIC)
    return cv2.GaussianBlur(img, (0, 0), 1.0)


@pytest.mark.parametrize("seed", range(6))
def test_eyes_finds_no_face_in_random_textures(seed):
    # Regression: frontalface_default with minNeighbors=5 saw faces here,
    # which would have reported "closed" eyes on a photo without people.
    assert features.EyeDetector.load().detect(texture(seed))["state"] == "no_face"


def test_eyes_reports_no_face_on_a_scene_without_people():
    result = features.EyeDetector.load().detect(scene())
    assert result["state"] == "no_face"
    assert result["faces"] == 0
    assert 0.0 <= result["confidence"] <= 1.0
