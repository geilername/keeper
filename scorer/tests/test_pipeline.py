"""The per-photo pipeline and the message handling rules, without NATS."""

import json

import cv2
import pytest

from keeper_scorer import features, pipeline, service
from test_features import scene

SHA = "a" * 64


@pytest.fixture(scope="module")
def detector():
    return features.EyeDetector.load()


def test_analyse_returns_photo_scored_shape(tmp_path, detector):
    path = tmp_path / "DSC0001.jpg"
    cv2.imwrite(str(path), scene())

    feats, timings = pipeline.analyse(path, detector)

    assert set(feats) == {"sharpness", "exposure", "eyes", "dhash"}
    assert set(timings) == {"decode", "faces", "sharpness", "exposure", "hash"}
    assert all(t >= 0 for t in timings.values())
    event = service.scored_event(
        {"sha256": SHA, "file_name": "DSC01234.ARW", "captured_at": "2026-11-09T10:00:00Z"}, feats, timings
    )
    decoded = json.loads(json.dumps(event))
    assert decoded["v"] == 1
    assert decoded["file_name"] == "DSC01234.ARW"  # passed through for Lightroom


def test_analyse_raises_for_missing_and_corrupt_files(tmp_path, detector):
    with pytest.raises(FileNotFoundError):
        pipeline.analyse(tmp_path / "gone.jpg", detector)
    bad = tmp_path / "bad.jpg"
    bad.write_bytes(b"\xff\xd8 truncated")
    with pytest.raises(features.DecodeError):
        pipeline.analyse(bad, detector)


def ingested(**overrides) -> bytes:
    event = {
        "v": 1,
        "sha256": SHA,
        "file_name": "DSC0001.jpg",
        "path": "/spool/photos/aa/DSC0001.jpg",
        "size_bytes": 123,
        "captured_at": "2026-11-09T10:00:00Z",
        "camera_model": "ILCE-7M4",
    }
    event.update(overrides)
    return json.dumps(event).encode()


def test_parse_ingested_accepts_a_valid_event(tmp_path):
    event = service.parse_ingested(ingested(path=str(tmp_path / "x.jpg")), tmp_path)
    assert event["path"] == tmp_path / "x.jpg"


@pytest.mark.parametrize(
    "data",
    [
        b"not json",
        b"[]",
        ingested(v=2),
        ingested(sha256="nope"),
        ingested(path="/etc/passwd"),
        ingested(path="/spool/../etc/passwd"),
    ],
    ids=["not-json", "not-object", "wrong-version", "bad-sha", "outside-spool", "traversal"],
)
def test_parse_ingested_rejects_unusable_events(data, tmp_path):
    with pytest.raises(service.Permanent):
        service.parse_ingested(data, tmp_path / "spool")


def test_nak_delay_backs_off_and_caps():
    assert [service.nak_delay(n) for n in range(1, 7)] == [2, 4, 8, 16, 30, 30]
