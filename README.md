# dualhd-mac

A macOS userspace driver for the **Hauppauge WinTV-dualHD** ATSC USB TV tuner,
with an HDHomeRun-compatible network tuner server so **Plex** (and other DVR
software that supports HDHomeRun) can watch and record over-the-air TV on a Mac.

Hauppauge provides no macOS driver for this tuner. This project talks to the
hardware directly over USB with libusb: no kernel extension, no root access.

- Both tuners work, independently and at the same time
- Full-rate ATSC 1.0 transport streams (19.39 Mbit/s per tuner)
- Channel scan with virtual channel names from the broadcast guide (PSIP)
- Emulates an HDHomeRun: discovery, `discover.json`, `lineup.json`,
  `/auto/v<channel>` streams filtered to the requested program
- Viewers of channels on the same RF frequency share a tuner
- Recovers automatically from unplugging/replugging and device resets
- Runs as a launchd agent at login

## Supported hardware

| USB ID      | Device                                         | Status   |
|-------------|------------------------------------------------|----------|
| `2040:826d` | Hauppauge WinTV-dualHD, model 01595 (ATSC/QAM) | Working  |

Inside: an Empia EM28274 USB bridge, two LG LGDT3306A demodulators and two
Silicon Labs Si2157 tuners. The tuners run from their built-in ROM firmware,
so no firmware files are needed.

Other Hauppauge models built from the same chips (`2040:026d`, `2040:826e`,
`2040:826f`, `2040:8270`, `2040:8271`) probably work but are untested; only
`2040:826d` is recognised for now.

**Not supported:** ATSC 3.0 ("NextGen TV") broadcasts, which this hardware
cannot decode; QAM cable; the DVB-T/T2/C variant of the dualHD; analog TV.

To check which model you have:

```sh
system_profiler SPUSBHostDataType | grep -B8 -A2 'USB Vendor ID: 0x2040'
```

Look for `USB Product ID: 0x826d`. (The device names itself `955D`.)

## Requirements

- macOS 13 or later, on Apple silicon or Intel
- Plex Media Server with an active **Plex Pass** (required by Plex for Live TV & DVR)

## Install

Download the latest release (a universal binary for Apple silicon and Intel,
with libusb built in). This unpacks into a `dualhd-mac` folder in the
current directory:

```sh
curl -fsSL https://github.com/mattrasband/dualhd-mac/releases/latest/download/dualhd-mac-macos-universal.tar.gz | tar xz
cd dualhd-mac
```

To upgrade, run the same command from the same place (your `lineup.json`
and settings are kept), then `deploy/install.sh` again.

The binaries aren't signed by Apple. Downloading with `curl` as above avoids
Gatekeeper's quarantine; if you downloaded the archive in a browser instead,
run `xattr -dr com.apple.quarantine .` in the unpacked folder.

To build from source instead, you need [Go](https://go.dev/dl/) 1.22 or later
and `brew install libusb pkgconf`:

```sh
git clone https://github.com/mattrasband/dualhd-mac
cd dualhd-mac
go build -o bin/ ./cmd/...
```

## Quick start

```sh
./bin/probe            # check the tuner is detected and the chips respond
./bin/scan             # scan RF channels 2-36, writes lineup.json (~10s)
deploy/install.sh      # install and start the server as a login service
```

Then in Plex: **Settings → Live TV & DVR → Set Up Plex DVR**. If the tuner
isn't found automatically, choose *"Don't see your HDHomeRun device? Enter
its network address manually"* and enter `127.0.0.1:5004`. Pick the
antenna/over-the-air guide for your ZIP code.

Only one program can use the tuner at a time, so stop the service before
running `scan`, `tune` or `probe`:

```sh
deploy/install.sh uninstall   # stop and remove
./bin/scan                    # e.g. rescan after moving the antenna
deploy/install.sh             # install again
```

Logs: `~/Library/Logs/dualhd-mac.log`

## Tools

| Command        | Purpose                                                          |
|----------------|------------------------------------------------------------------|
| `bin/probe`    | Bring up the bridge and list I2C devices and chip IDs            |
| `bin/scan`     | Scan RF channels with both tuners; writes `lineup.json`          |
| `bin/tune`     | Tune one or two RF channels, print signal stats, record to `.ts` |
| `bin/hdhr`     | HDHomeRun-compatible server (what the launchd agent runs)        |
| `bin/usbdesc`  | Print the device's USB interfaces and endpoints                  |

```sh
./bin/tune -rf 35 -d 0           # tune RF 35, show RSSI/SNR/lock, no capture
./bin/tune -rf 35,33 -d 30s      # record both tuners at once to rf35.ts, rf33.ts
```

`tune` takes physical **RF** channel numbers; `scan` shows which virtual
channels (e.g. 4.1) each RF channel carries.

## How it works

```
USB ──> em28xx ──> lgdt3306a + si2157 ──> dualhd ──> tuner ──> tsfilter ──> hdhr ──> Plex
        bridge,    demod + tuner (x2)       board,     sharing,   one          HTTP +
        I2C                                 TS bulk    health     program      discovery
```

- `em28xx/`: USB vendor register access and the bridge's I2C master
- `lgdt3306a/`, `si2157/`: demodulator and tuner setup for 8VSB
- `dualhd/`: board setup (GPIO resets, dual TS), tuning by RF channel,
  bulk transport stream capture
- `tuner/`: shares the two tuners between viewers; detects disconnects, stalls
  and resets and reopens the device
- `psip/`: PAT and ATSC virtual channel table parsing
- `tsfilter/`: reduces a transport to one program (as an HDHomeRun does)
- `hdhr/`: HDHomeRun HTTP API and UDP discovery

The hardware programming is ported from the Linux kernel's `em28xx`,
`lgdt3306a` and `si2157` drivers. Hauppauge's Windows driver was not
disassembled or used, and no proprietary firmware is included.

## Tests

```sh
go test ./...                                      # no hardware needed
HAUPPAUGE_HW=1 go test -v -run Reconnect ./tuner   # needs the tuner; stop the service first
```

## Releases

Pushing a tag such as `v0.1.0` runs `.github/workflows/release.yml`, which
builds the package with `scripts/build-release.sh` and publishes a GitHub
release. The script also works locally and writes to `dist/`.

## Limitations

- ATSC 1.0 only (the hardware can't decode ATSC 3.0)
- The channel lineup comes from `scan`; Plex's "scan channels" button just
  re-reads it
- HTTP has no authentication; the server listens on all interfaces at
  port 5004, as an HDHomeRun does. Use `-addr 127.0.0.1:5004` (and add the
  tuner to Plex manually) to restrict it to this Mac.

## License

GPL-2.0-or-later; see [LICENSE](LICENSE). The driver code is derived from the
Linux kernel drivers listed above, and keeps their authors' copyright notices.
Builds that include the Apache-2.0 [gousb](https://github.com/google/gousb)
library are distributed under GPL version 3.

## Trademarks

Hauppauge and WinTV are trademarks of Hauppauge Computer Works. HDHomeRun is a
trademark of SiliconDust Engineering. Plex is a trademark of Plex, Inc. This
project is not affiliated with or endorsed by any of them. The server
identifies itself to clients as an HDHomeRun-compatible device for
interoperability only.
