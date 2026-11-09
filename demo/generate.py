"""Builds the demo burst sets from the CC0 photos in demo/photos/.

Each set imitates a 6-frame handheld burst of one scene: every frame is a
slightly shifted and zoomed crop of the same photo, and one or two frames are
clearly degraded (motion blur, under- or overexposure), so the culler has an
obvious best frame and obvious rejects. Frames get fresh EXIF only
(DateTimeOriginal + SubSecTimeOriginal 0.3 s apart, Model=KeeperSim); the
source photos' own EXIF, including any GPS data, is dropped.

Output: demo/sets/<set>/<set>-NN.jpg plus expected.json, which records what
each frame is meant to be, for checking the scorer by eye. Deterministic:
same sources + same pinned Pillow = same bytes.
"""

from __future__ import annotations

import json
import random
from datetime import datetime, timedelta
from pathlib import Path

from PIL import Image, ImageEnhance, ImageOps

ROOT = Path(__file__).resolve().parent
PHOTOS = ROOT / "photos"
SETS = ROOT / "sets"

OUT_W, OUT_H = 1800, 1200  # 3:2 like a camera; ~0.4-0.9 MB at q88
QUALITY = 88
FRAME_GAP = timedelta(milliseconds=300)
MAX_BYTES = 2 * 1024 * 1024

# Exif tag numbers (Pillow's ExifTags.Base / IFD names, spelled out so the
# file reads without a lookup table).
MAKE, MODEL, SOFTWARE, DATETIME = 0x010F, 0x0110, 0x0131, 0x0132
EXIF_IFD = 0x8769
DATETIME_ORIGINAL, OFFSET_TIME_ORIGINAL, SUBSEC_TIME_ORIGINAL = 0x9003, 0x9011, 0x9291

# name -> source photo, crop (fractions of the source: centre x, centre y,
# width), first capture time, and the frame kinds in burst order.
BURSTS = {
    "beach-dog": {
        "source": "beach.jpg",
        "crop": (0.5, 0.5, 0.80),
        "start": datetime(2026, 11, 9, 10, 0, 0),
        "frames": ["sharp", "sharp", "blur", "sharp", "under", "sharp"],
    },
    "road-dog": {
        "source": "road.jpg",
        "crop": (0.5, 0.5, 0.80),
        "start": datetime(2026, 11, 9, 10, 5, 0),
        "frames": ["sharp", "over", "sharp", "sharp", "blur", "sharp"],
    },
    "island-dog": {
        "source": "island.jpg",
        "crop": (0.5, 0.5, 0.80),
        "start": datetime(2026, 11, 9, 10, 10, 0),
        "frames": ["sharp", "sharp", "under", "sharp", "sharp", "blur"],
    },
}


def frame_crop(src: Image.Image, crop: tuple[float, float, float], rng: random.Random,
               extra_w: int = 0) -> Image.Image:
    """Crops a 3:2 window with handheld jitter and resizes it to the output
    size (plus extra_w pixels of width, used as room for motion blur)."""
    cx, cy, wf = crop
    sw, sh = src.size
    zoom = rng.uniform(1.0, 1.04)
    w = min(sw * wf / zoom, sh * 1.5)
    h = w / 1.5
    cx = cx * sw + rng.uniform(-0.015, 0.015) * w
    cy = cy * sh + rng.uniform(-0.015, 0.015) * h
    left = min(max(cx - w / 2, 0), sw - w)
    top = min(max(cy - h / 2, 0), sh - h)
    scale = OUT_W / w
    extra_src = extra_w / scale
    right = min(left + w + extra_src, sw)
    box = (round(left), round(top), round(right), round(top + h))
    out_w = round((box[2] - box[0]) * scale)
    return src.resize((out_w, OUT_H), Image.Resampling.LANCZOS, box=box)


def motion_blur(wide: Image.Image, length: int) -> Image.Image:
    """Horizontal camera shake: the average of `length` shifted windows."""
    acc = wide.crop((0, 0, OUT_W, OUT_H))
    for k in range(1, length):
        acc = Image.blend(acc, wide.crop((k, 0, k + OUT_W, OUT_H)), 1 / (k + 1))
    return acc


def render(src: Image.Image, crop, kind: str, rng: random.Random) -> Image.Image:
    if kind == "blur":
        length = 40
        return motion_blur(frame_crop(src, crop, rng, extra_w=length), length)
    img = frame_crop(src, crop, rng)
    if kind == "under":
        return ImageEnhance.Brightness(img).enhance(0.28)
    if kind == "over":
        return ImageEnhance.Brightness(img).enhance(2.3)
    return img


def exif_for(t: datetime) -> Image.Exif:
    exif = Image.Exif()
    stamp = t.strftime("%Y:%m:%d %H:%M:%S")
    exif[MAKE] = "Keeper"
    exif[MODEL] = "KeeperSim"
    exif[SOFTWARE] = "keeper demo/generate.py"
    exif[DATETIME] = stamp
    ifd = exif.get_ifd(EXIF_IFD)
    ifd[DATETIME_ORIGINAL] = stamp
    ifd[SUBSEC_TIME_ORIGINAL] = f"{t.microsecond // 1000:03d}"
    ifd[OFFSET_TIME_ORIGINAL] = "+01:00"
    return exif


def build(name: str, spec: dict) -> None:
    src = ImageOps.exif_transpose(Image.open(PHOTOS / spec["source"])).convert("RGB")
    rng = random.Random(name)  # fixed seed per set: same jitter every run
    out = SETS / name
    out.mkdir(parents=True, exist_ok=True)
    for old in out.glob(f"{name}-*.jpg"):
        old.unlink()

    expected = []
    for i, kind in enumerate(spec["frames"]):
        t = spec["start"] + i * FRAME_GAP
        path = out / f"{name}-{i + 1:02d}.jpg"
        render(src, spec["crop"], kind, rng).save(
            path, "JPEG", quality=QUALITY, optimize=True, exif=exif_for(t).tobytes())
        size = path.stat().st_size
        if size > MAX_BYTES:
            raise SystemExit(f"{path} is {size} bytes, over the 2 MB budget")
        expected.append({"file": path.name, "kind": kind,
                         "captured_at": t.isoformat(timespec="milliseconds") + "+01:00"})
        print(f"{path.relative_to(ROOT)}  {kind:5}  {size // 1024} KiB")

    (out / "expected.json").write_text(json.dumps(
        {"set": name, "source": spec["source"], "frames": expected}, indent=2) + "\n")


def main() -> None:
    for name, spec in BURSTS.items():
        build(name, spec)


if __name__ == "__main__":
    main()
