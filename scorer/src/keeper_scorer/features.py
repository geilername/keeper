"""Pure image-feature functions. No NATS, no OpenTelemetry: easy to unit test.

Every function takes a downscaled 8-bit grayscale image (numpy uint8, HxW).
All features are computed on the same fixed working size, so scores are
comparable between photos whatever the camera resolution.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

import cv2
import numpy as np

# Long edge of the working image. Big enough for faces and blur, small enough
# that a 24 MP photo scores in well under a second.
WORK_LONG_EDGE = 1024

# Laplacian variance at which the sharpness score is 0.5. ~100 is the usual
# "blurry below this" rule of thumb at roughly 1 MP.
SHARPNESS_HALF_POINT = 100.0

# Pixel values counted as clipped highlights / crushed shadows.
HIGHLIGHT_CLIP = 250
SHADOW_CLIP = 5


class DecodeError(Exception):
    """The file exists but is not a decodable image (corrupt, truncated, not an image)."""


def decode(data: bytes, long_edge: int = WORK_LONG_EDGE) -> np.ndarray:
    """Decode image bytes to grayscale and downscale so the long edge <= long_edge.

    IMREAD_GRAYSCALE also applies the EXIF orientation, so faces are upright.
    """
    if not data:
        raise DecodeError("empty file")
    img = cv2.imdecode(np.frombuffer(data, dtype=np.uint8), cv2.IMREAD_GRAYSCALE)
    if img is None:
        raise DecodeError("not a decodable image")
    return downscale(img, long_edge)


def downscale(gray: np.ndarray, long_edge: int = WORK_LONG_EDGE) -> np.ndarray:
    h, w = gray.shape[:2]
    scale = long_edge / max(h, w)
    if scale >= 1.0:
        return gray
    # INTER_AREA averages pixels: the right filter for shrinking.
    return cv2.resize(gray, (round(w * scale), round(h * scale)), interpolation=cv2.INTER_AREA)


def sharpness(gray: np.ndarray) -> dict:
    """Variance of the Laplacian (edge energy). Blur removes edges, so it drops.

    score = v / (v + k): 0 for a flat image, 0.5 at k, approaching 1 when sharp.
    """
    variance = float(cv2.Laplacian(gray, cv2.CV_64F).var())
    score = variance / (variance + SHARPNESS_HALF_POINT)
    return {"variance": round(variance, 2), "score": round(score, 4)}


def exposure(gray: np.ndarray) -> dict:
    """Mean luminance plus clipped-highlight and crushed-shadow fractions.

    score = closeness of the mean to mid-grey, times a penalty for clipping:
    1.0 = mean at 0.5 and nothing clipped; 0 = black/white frame or >=50 % clipped.
    """
    mean = float(gray.mean()) / 255.0
    n = gray.size
    highlights = float(np.count_nonzero(gray >= HIGHLIGHT_CLIP)) / n
    shadows = float(np.count_nonzero(gray <= SHADOW_CLIP)) / n
    centred = 1.0 - abs(mean - 0.5) * 2.0
    clip_penalty = min(1.0, (highlights + shadows) * 2.0)
    score = max(0.0, centred * (1.0 - clip_penalty))
    return {
        "mean": round(mean, 4),
        "highlights_clipped": round(highlights, 4),
        "shadows_clipped": round(shadows, 4),
        "score": round(score, 4),
    }


def dhash(gray: np.ndarray) -> str:
    """64-bit difference hash as 16 hex chars.

    Shrink to 9x8, then one bit per pixel: "is it brighter than its left
    neighbour?". Near-identical frames (burst neighbours) differ in few bits.
    """
    small = cv2.resize(gray, (9, 8), interpolation=cv2.INTER_AREA)
    bits = (small[:, 1:] > small[:, :-1]).flatten()
    value = 0
    for bit in bits:
        value = (value << 1) | int(bit)
    return f"{value:016x}"


def hamming(a: str, b: str) -> int:
    """Number of differing bits between two dhash hex strings."""
    return (int(a, 16) ^ int(b, 16)).bit_count()


@dataclass(frozen=True)
class EyeDetector:
    """Haar cascades from opencv-python-headless: small, CPU only, no model download.

    The eye cascade is trained on open eyes, so "face found but no eyes found"
    is our signal for closed eyes. It is a heuristic and the confidence says so.

    A false face turns into "closed" (a reject), so the face detector is tuned
    for precision: frontalface_alt2 with minNeighbors=10 found 0 "faces" in 36
    random-texture images, frontalface_default with 5 found 18.
    """

    face: cv2.CascadeClassifier
    eye: cv2.CascadeClassifier

    @classmethod
    def load(cls) -> EyeDetector:
        base = Path(cv2.data.haarcascades)
        face = cv2.CascadeClassifier(str(base / "haarcascade_frontalface_alt2.xml"))
        eye = cv2.CascadeClassifier(str(base / "haarcascade_eye.xml"))
        if face.empty() or eye.empty():
            raise RuntimeError(f"Haar cascades not found in {base}")
        return cls(face, eye)

    def detect(self, gray: np.ndarray) -> dict:
        eq = cv2.equalizeHist(gray)
        min_face = max(40, min(gray.shape[:2]) // 12)
        faces = self.face.detectMultiScale(eq, scaleFactor=1.1, minNeighbors=10, minSize=(min_face, min_face))
        if len(faces) == 0:
            return {"state": "no_face", "confidence": 0.7, "faces": 0}

        # Judge the largest face: in a burst that is the subject.
        x, y, w, h = max(faces, key=lambda f: int(f[2]) * int(f[3]))
        # Eyes sit in the upper half of the face box.
        roi = eq[y : y + h // 2, x : x + w]
        min_eye = max(8, w // 10)
        eyes = self.eye.detectMultiScale(roi, scaleFactor=1.1, minNeighbors=6, minSize=(min_eye, min_eye))
        n_eyes = min(len(eyes), 2)
        state, confidence = {
            2: ("open", 0.9),  # both eyes found
            1: ("open", 0.6),  # one eye found: profile, hair, glasses glare
            0: ("closed", 0.6),  # face but no open eye; could also be blur or sunglasses
        }[n_eyes]
        return {"state": state, "confidence": confidence, "faces": int(len(faces))}
