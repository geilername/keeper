#!/bin/sh
# Downloads the CC0 source photos for the demo bursts into demo/photos/
# (gitignored) and checks each against the SHA-1 that Wikimedia Commons
# publishes for the file. Licences and authors: demo/SOURCES.md.
#
# Usually run through ./generate.sh (inside the generator container), but it
# only needs a POSIX shell, curl or wget, and sha1sum or shasum.
set -eu

cd "$(dirname "$0")"
mkdir -p photos

# Wikimedia asks automated clients for a descriptive User-Agent.
UA="keeper-demo-fetch/1.0 (hackathon demo; https://github.com/geilername/keeper)"

sha1() {
	if command -v sha1sum >/dev/null 2>&1; then sha1sum "$1" | cut -d' ' -f1
	else shasum -a 1 "$1" | cut -d' ' -f1; fi
}

download() { # url dest; resumes a partial file, up to 5 attempts
	for attempt in 1 2 3 4 5; do
		if command -v curl >/dev/null 2>&1; then
			curl -fsSL -A "$UA" -C - -o "$2" "$1" && return 0
		else
			wget -q -c -U "$UA" -O "$2" "$1" && return 0
		fi
		echo "        attempt $attempt failed, retrying" >&2
		sleep 2
	done
	return 1
}

# local name | SHA-1 from the Commons API | original file URL
while IFS='|' read -r name want url; do
	[ -n "$name" ] || continue
	dest="photos/$name"
	if [ -f "$dest" ] && [ "$(sha1 "$dest")" = "$want" ]; then
		echo "ok      $dest"
		continue
	fi
	echo "fetch   $dest"
	download "$url" "$dest.part"
	got=$(sha1 "$dest.part")
	if [ "$got" != "$want" ]; then
		rm -f "$dest.part"
		echo "sha1 mismatch for $name: got $got, want $want" >&2
		exit 1
	fi
	mv "$dest.part" "$dest"
done <<'EOF'
beach.jpg|15636426634b8ab942ee9b2fbfef1819ae8d1018|https://upload.wikimedia.org/wikipedia/commons/0/0d/Dog_at_N%C3%B8rre_Vorup%C3%B8r_Strand.jpg
road.jpg|68e7eaad2d031b5a5bbd9c84cdce8464128054e6|https://upload.wikimedia.org/wikipedia/commons/3/36/Dog_on_dirt_road.jpg
island.jpg|aadb249b8d08f0318f1755141f14c888d9a444c7|https://upload.wikimedia.org/wikipedia/commons/2/2b/Dog_in_Chacachacare.jpg
EOF
