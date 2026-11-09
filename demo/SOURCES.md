# Demo photo sources

The generated bursts in `sets/beach-dog`, `sets/road-dog` and `sets/island-dog`
are derived from these photos from Wikimedia Commons. Each one is published
under **CC0 1.0** (Creative Commons Zero, Public Domain Dedication,
<https://creativecommons.org/publicdomain/zero/1.0/>), as "own work" of the
listed author. No people are recognisable in any of them.

`fetch.sh` downloads the originals into `photos/` (gitignored) and checks
each file against the SHA-1 that Commons publishes for it. `generate.py`
crops and degrades them and writes new EXIF; none of the original EXIF
(camera, GPS) is carried over.

| Local file | Set | Commons page | Author | Licence | SHA-1 |
|---|---|---|---|---|---|
| `photos/beach.jpg` | `beach-dog` | [Dog at Nørre Vorupør Strand.jpg](https://commons.wikimedia.org/wiki/File:Dog_at_N%C3%B8rre_Vorup%C3%B8r_Strand.jpg) | W.carter | CC0 1.0 | `15636426634b8ab942ee9b2fbfef1819ae8d1018` |
| `photos/road.jpg` | `road-dog` | [Dog on dirt road.jpg](https://commons.wikimedia.org/wiki/File:Dog_on_dirt_road.jpg) | Arlo Barnes | CC0 1.0 | `68e7eaad2d031b5a5bbd9c84cdce8464128054e6` |
| `photos/island.jpg` | `island-dog` | [Dog in Chacachacare.jpg](https://commons.wikimedia.org/wiki/File:Dog_in_Chacachacare.jpg) | Wilfredor | CC0 1.0 | `aadb249b8d08f0318f1755141f14c888d9a444c7` |

Licence data checked on 2026-11-09 through the Commons API
(`extmetadata.LicenseShortName = CC0`). CC0 requires no attribution; it is
recorded here anyway.

## Rebuilding

```sh
demo/generate.sh
```

This builds a small pinned image (`python:3.13.9-alpine3.22` +
`Pillow==13.0.0`), runs `fetch.sh` and then `generate.py` in it, and writes
the files as your user. The output is byte-identical between runs.
