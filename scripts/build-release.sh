#!/bin/sh
# Builds release packages: universal (arm64 + x86_64) binaries with libusb
# linked statically, so users don't need Homebrew or Go.
#
#   scripts/build-release.sh [version]     e.g. v0.1.0 (default: git describe)
#
# Output: dist/dualhd-mac-macos-universal.tar.gz (unpacking to dualhd-mac/)
# and a .sha256 file. The name has no version so that
# releases/latest/download/<name> always works.
# Needs Xcode command line tools and Go; libusb is built from source.
set -eu

LIBUSB_VERSION=1.0.30
LIBUSB_SHA256=fea36f34f9156400209595e300840767ab1a385ede1dc7ee893015aea9c6dbaf
MIN_MACOS=13.0
COMMANDS="hdhr scan tune probe usbdesc"

ROOT=$(cd "$(dirname "$0")/.." && pwd)
VERSION=${1:-$(git -C "$ROOT" describe --tags --always --dirty)}
BUILD="$ROOT/build"
DIST="$ROOT/dist"
NAME=dualhd-mac
ARCHIVE=dualhd-mac-macos-universal.tar.gz

export MACOSX_DEPLOYMENT_TARGET=$MIN_MACOS

rm -rf "$BUILD" "$DIST"
mkdir -p "$BUILD" "$DIST"
cd "$BUILD"

tarball=libusb-$LIBUSB_VERSION.tar.bz2
curl -fsSLO "https://github.com/libusb/libusb/releases/download/v$LIBUSB_VERSION/$tarball"
echo "$LIBUSB_SHA256  $tarball" | shasum -a 256 -c -
tar xjf "$tarball"

for arch in arm64 x86_64; do
	prefix="$BUILD/libusb-$arch"
	src="$BUILD/libusb-src-$arch"
	cp -R "libusb-$LIBUSB_VERSION" "$src"
	(
		cd "$src"
		# Newer SDKs declare pipe2(), which then links weakly for older
		# deployment targets and crashes (null call) where it's missing.
		./configure --quiet --host="$arch-apple-darwin" --prefix="$prefix" \
			--disable-shared --enable-static ac_cv_func_pipe2=no \
			CC="clang -arch $arch" CFLAGS="-O2 -mmacosx-version-min=$MIN_MACOS"
		make -s -j"$(sysctl -n hw.ncpu)" install >/dev/null
	)
	# Only the static library is installed, so pkg-config's -lusb-1.0
	# links it statically. Add the frameworks libusb needs.
	sed -i '' 's|^Libs:.*|& -lobjc -Wl,-framework,IOKit -Wl,-framework,CoreFoundation -Wl,-framework,Security|' \
		"$prefix/lib/pkgconfig/libusb-1.0.pc"

	case $arch in arm64) goarch=arm64 ;; x86_64) goarch=amd64 ;; esac
	for cmd in $COMMANDS; do
		(
			cd "$ROOT"
			CGO_ENABLED=1 GOOS=darwin GOARCH=$goarch CC="clang -arch $arch" \
				PKG_CONFIG_PATH="$prefix/lib/pkgconfig" PKG_CONFIG_LIBDIR="$prefix/lib/pkgconfig" \
				go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
				-o "$BUILD/bin-$arch/$cmd" "./cmd/$cmd"
		)
	done
done

pkg="$BUILD/$NAME"
mkdir -p "$pkg/bin" "$pkg/deploy"
for cmd in $COMMANDS; do
	lipo -create -output "$pkg/bin/$cmd" "$BUILD/bin-arm64/$cmd" "$BUILD/bin-x86_64/$cmd"
	if otool -L "$pkg/bin/$cmd" | grep -q libusb; then
		echo "$cmd links libusb dynamically" >&2
		exit 1
	fi
	# A weak libSystem import means a function that may not exist on the
	# minimum macOS version (see pipe2 above).
	if nm -m "$pkg/bin/$cmd" | grep 'weak external' | grep -q 'from libSystem'; then
		echo "$cmd has weak libSystem imports:" >&2
		nm -m "$pkg/bin/$cmd" | grep 'weak external' | grep 'from libSystem' >&2
		exit 1
	fi
done
cp "$ROOT/deploy/install.sh" "$pkg/deploy/"
cp "$ROOT/README.md" "$ROOT/LICENSE" "$pkg/"

echo "$VERSION" > "$pkg/VERSION"

tar -C "$BUILD" -czf "$DIST/$ARCHIVE" "$NAME"
(cd "$DIST" && shasum -a 256 "$ARCHIVE" > "$ARCHIVE.sha256")
lipo -info "$pkg/bin/hdhr"
echo "built dist/$ARCHIVE ($VERSION)"
