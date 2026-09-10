<div align="center">

# LiuGuang YiYing (流光逸影)

**Home private cinema: scan with your phone, or use DLNA on your TV — play movies right off your PC.**

English | [简体中文](README.md)

<img src="public/banner.jpg" alt="LiuGuang YiYing product banner: phone scanning QR, TV media center, and desktop console" width="880">

![Go](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white)
![Platform](https://img.shields.io/badge/platform-Windows-blue)
![License](https://img.shields.io/badge/license-MIT-green)

**Single binary** · **Scan & play** · **DLNA built-in** · **One JSON config**

</div>

---

Run one exe and your PC becomes a home media server — open the library by scanning a QR code on your phone, or discover it natively on smart TVs via DLNA. No app installation required.

## ✨ Features

- **Single binary, zero install**: one exe does it all — web pages are embedded in the binary, and the only config is a JSON file
- **Scan & play on phones**: the desktop console shows a QR code; open it in any mobile browser and enjoy the library with full seek support
- **DLNA for TVs**: smart TVs and set-top boxes discover it automatically in their built-in "Media Center"; MKV and H.265 play at original quality, decoded locally by the TV
- **Smart transcoding**: MP4 streams directly at zero cost; formats a browser can't handle are remuxed/transcoded on the fly via ffmpeg
- **Point & share**: pick folders with your mouse in the desktop console; changes apply instantly and persist
- **Stable playback**: MSE buffer watermark, stall watchdog, seek-triggered repositioning and auto-recovery

## 🚀 Quick Start

**Option 1: Download a release** (recommended)

<!-- TODO(user): confirm the Release link is live once published -->
Download `liuguang-yiying.exe` from [Releases](https://github.com/woyin2024/lengyi-movielan/releases) and double-click to run.

**Option 2: Build from source** (Go 1.26+)

```bash
git clone https://github.com/woyin2024/lengyi-movielan.git
cd lengyi-movielan
go build -trimpath -ldflags "-s -w" -o liuguang-yiying.exe
```

Double-click `liuguang-yiying.exe`. The desktop console opens automatically and you'll see:

```text
============================================
  流光逸影 started
  Phone browser:   http://192.168.1.3:8090
  Desktop console: http://localhost:8090/pc
  TV/Box: find "流光逸影" in your TV's DLNA / Media Center list
============================================
```

Connect your phone to the same Wi-Fi, scan the code or type the URL, and start watching.

## 📖 Usage

### Config file `config.json`

Generated automatically on first run, or copy it from `config.example.json`:

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `dirs` | string array | `["."]` | Movie folders to share; multiple entries and whole drives (e.g. `E:\`) are supported |
| `port` | number | `8090` | Listening port; if occupied, the app reports "already running" |
| `token` | string | `""` | Access token; empty disables auth. When set, web pages require `?key=<token>` |

You can add or remove shared folders from the phone UI ("目录") or the desktop console; changes apply instantly and are written back to the config.

### Optional dependency: ffmpeg

- Playing MKV / H.265 in mobile browsers requires `ffmpeg.exe` (with `ffprobe.exe`) — put it next to the exe or in your PATH
- It works without it: direct MP4 playback is unaffected, and DLNA playback streams original files decoded locally by the TV — no ffmpeg needed at all

### Watching on a TV / set-top box

Connect the TV to the same network as the PC, then find "流光逸影" in the TV's built-in "Media Center", "File Manager" or any DLNA app. If it doesn't show up, make sure the Windows Firewall allows the program (click "Allow" on the first-run prompt; UDP port 1900 must be covered).

## 🔒 Security Notes

This project is designed as a **self-hosted tool for trusted home networks**. Before using it, please understand:

- **Never expose the port to the public internet.** The project is not designed to withstand public-network attacks.
- **The token is weak protection**: it travels in URL parameters and uses a non-constant-time comparison. It prevents accidental access by family members, not attackers.
- **DLNA has no authentication (protocol limitation)**: even with a token set, any device on the LAN can still browse the movie list and obtain playable links via DLNA. If that concerns you, avoid untrusted networks (shared apartments, offices), or block inbound UDP port 1900 in your firewall to disable TV discovery.
- **Share movie folders, not whole drives**: sharing an entire drive (e.g. `E:\`) exposes every file on it to the LAN.
- **The desktop console is local-only**: the `/pc` console and disk browser only accept connections from the local machine; other LAN devices cannot change sharing settings.

## ❓ FAQ

<details>
<summary>MKV playback stutters or shows a black screen on the phone?</summary>

Make sure `ffmpeg` / `ffprobe` are next to the exe (or in PATH). If it still fails, use the "Copy link to VLC" button on the player page and paste it into VLC.
</details>

<details>
<summary>The TV can't find "流光逸影"?</summary>

Confirm the TV and PC are on the same subnet; allow the program in Windows Firewall on the first-run prompt; disable AP isolation on your router if it's enabled.
</details>

<details>
<summary>Port 8090 is already in use?</summary>

Change `port` in `config.json` and restart. The program detects an occupied port, reports "already running" and opens the console instead of launching a duplicate instance.
</details>

## 👤 About the Author

**冷逸 (LengYi)** — blogger behind **沃垠AI**, one of China's top AI tech media outlets; a Vibe Coding developer who "can't really code", obsessed with prompts, skills and agents. Product & operations background, focused on in-depth AI reviews.

- **Same handle everywhere**: 沃垠AI (WeChat · Xiaohongshu · Zhihu · GitHub · Bilibili · X)
- Follow the WeChat official account for AI reviews and Vibe Coding walkthroughs:

<div align="left">
<img src="public/wechat-qr.png" alt="沃垠AI WeChat official account QR code" width="160">
</div>

## 🤝 Contributing

Issues and PRs are welcome: when reporting a bug, please attach the startup log and reproduction steps. When submitting code, keep the single-binary, stdlib-first philosophy.

## 📄 License

[MIT](LICENSE) © 冷逸
