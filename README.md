# iphone-mcp

An MCP server that lets an AI agent see and touch a real iPhone over USB or Wi-Fi. It talks to the
phone's developer services through [go-ios](https://github.com/danielpaulus/go-ios); nothing is
installed on the phone.

The agent looks at a half-size screenshot, taps and swipes in that image's pixels, pastes text
through the clipboard, and opens or quits apps.

## Tools

| Tool | What it does |
|------|--------------|
| `look` | Screenshot at half size. Every x/y the other tools take is a pixel of this image. |
| `tap` | Tap at (x, y) and return the new screen. Refused (the new screen comes back instead) if the area around the point changed since the last screenshot. |
| `long_press` | Press and hold at (x, y), default 800 ms. Guarded like `tap`. |
| `swipe` | Drag from (x1, y1) to (x2, y2), default 300 ms: scroll lists, turn pages, move picker wheels. |
| `type_text` | Put text (any language) on the iPhone's clipboard; the agent then pastes it into a field. |
| `open_app` | Quit and relaunch an app by bundle id or home-screen name, then return the screen. |
| `close_app` | Quit an app. |
| `list_apps` | Installed apps (name and bundle id), optionally filtered. |

The display stream and touch session open on the first touch and close after 30 s without one. A
lock file serialises touch across processes, so two agents never drive the phone at once.

## Requirements

- macOS (the only platform tested) and Go 1.26 or later.
- An iPhone on iOS 27 with Developer Mode on, trusted by this Mac. Touch uses the CoreDevice display
  and HID services of the iOS 27 developer disk image.
- The go-ios tunnel agent running (below).
- The phone unlocked while the agent works.

## Build

```sh
git clone https://github.com/Daily-AC/iphone-mcp
cd iphone-mcp
go build -o phone .
```

## Start the tunnel

Build the go-ios CLI from the commit this repo pins. For Wi-Fi, also apply `go-ios-wifi.patch`:
stock go-ios only tunnels phones on USB; the patch tunnels phones seen over Wi-Fi too and prefers
the cable when both are there.

```sh
git clone https://github.com/danielpaulus/go-ios
cd go-ios
git checkout ca2c56d2c294
git apply ../iphone-mcp/go-ios-wifi.patch   # optional: Wi-Fi
go build -o ios .
```

With the phone plugged in and unlocked:

```sh
mkdir -p ~/.local/share/iphone-mcp/pair
./ios tunnel start --userspace --pair-record-path="$HOME/.local/share/iphone-mcp/pair"
```

Leave it running (no sudo needed in userspace mode). In another terminal, mount the developer disk
image. The phone forgets it on every reboot, and mounting needs the phone unlocked.
go-ios's default image (`ios image auto`) has neither the display nor the HID service on iOS 27.
Mount the iOS 27 personalized image instead (`Image.dmg`, `Image.dmg.trustcache` and
`BuildManifest.plist`, build 27A5228h) from
[doronz88/DeveloperDiskImage](https://github.com/doronz88/DeveloperDiskImage/tree/main/PersonalizedImages/Xcode_iOS_DDI_Personalized):

```sh
./ios image mount --path=/path/to/Xcode_iOS_DDI_Personalized
./ios rsd ls | grep displayservice   # touch needs this service
```

If another image is already mounted, run `./ios image unmount` first.

For Wi-Fi, turn on lockdown over Wi-Fi once while the phone is on USB (the same switch as Finder's
"Show this iPhone when on Wi-Fi"), then unplug. From the iphone-mcp directory:

```sh
./phone wifi on
```

Check the whole chain with `./phone shot screen.png`.

## Register with Claude Code

```sh
claude mcp add iphone -- /path/to/iphone-mcp/phone mcp
```

Or in `.mcp.json`:

```json
{
  "mcpServers": {
    "iphone": { "command": "/path/to/iphone-mcp/phone", "args": ["mcp"] }
  }
}
```

Environment variables:

- `GO_IOS_UDID`: which phone to use when more than one is connected.
- `IPHONE_MCP_LOCK`: the touch lock file, default `~/.local/share/iphone-mcp/touch.lock`. Point
  every process that drives the phone at the same file.

## Command line

```
phone shot FILE           screenshot (PNG, full resolution)
phone tap X Y [X Y ...]   tap full-resolution pixels
phone do STEP...          one session for a sequence: tap X Y | swipe X1 Y1 X2 Y2 STEPS | wait MS | shot FILE
phone serve               JSON lines on stdin and stdout, same session as the MCP server (see serve.go)
phone wifi on|off|status  lockdown over Wi-Fi; needs USB
```

The command line uses full-resolution pixels; `serve` and the MCP tools use the half-size screenshot.

## Safety

An agent with these tools can do anything a person holding the unlocked phone can do.

- The tool descriptions tell the agent that sending a message, paying, deleting, or anything else
  that cannot be taken back needs the user's explicit go-ahead in the conversation first. This is
  an instruction to the model, not enforcement: the server carries out every call it receives.
  Watch what the agent does, or have your client ask before each tool call.
- `tap` and `long_press` refuse to touch a spot whose surroundings changed since the agent's last
  screenshot, so the agent does not hit something it never saw.

## Known limits

- No home or app-switcher gesture: switch apps with `open_app`, quit them with `close_app`.
- A passcode cannot be typed. Someone has to unlock the phone; `look` says when it is locked.
- No keyboard typing: text goes through the clipboard and is pasted.
- MCP coordinates are pixels of the half-size screenshot (603×1311 on a 1206×2622 screen).
- The screen size is hard-coded to 1206×2622 (`screenW`, `screenH` in `main.go`). On a phone with
  another resolution, change them or touches land in the wrong place.
- Tested with one iPhone on iOS 27.

## License

MIT, see [LICENSE](LICENSE). `go-ios-wifi.patch` modifies go-ios, which is MIT licensed by Daniel
Paulus.
