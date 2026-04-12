[![Go Version](https://img.shields.io/github/go-mod/go-version/AndreaLlerena2003/photoboot-picstop)](https://golang.org)
[![Platform: Windows](https://img.shields.io/badge/platform-windows-blue)](https://www.microsoft.com/windows)
[![Platform: macOS](https://img.shields.io/badge/platform-macOS-lightgrey)](https://www.apple.com/macos)

A high-performance backend service for Canon camera control, designed for photobooth applications. This service provides
a RESTful API to capture photos, a real-time MJPEG stream for live preview, and a **3D LUT filter pipeline** that applies
colour grading to both the preview stream and captured photos — all using the **Canon EDSDK**.

---

## 🚀 Features

- **📸 Instant Capture**: Trigger photo capture via HTTP POST with configurable timeouts.
- **🎥 Live Preview**: High-speed MJPEG stream (Electronic Viewfinder - EVF) compatible with standard `<img>` tags.
- **🎨 Real-Time Filters**: 3D LUT colour grading applied to both the live preview and captured photos. Drop any `.cube` file into `filters/` — no restart needed.
- **🔄 Auto-Reconnection**: Seamlessly handles camera disconnections and reconnections without service restarts.
- **🏗️ Clean Architecture**: Decoupled domain logic and infrastructure, making the codebase maintainable and testable.
- **⚡ Performance-First**: Uses optimised CGo bindings for direct communication with the Canon EDSDK. LUT applied with parallel row-striping on capture; pooled buffers on preview.

---

## 📋 Requirements

This project supports **Windows** and **macOS** (Darwin) environments.

### General Requirements

- **Go**: Version 1.20+ with `CGO_ENABLED=1`.
- **Canon EDSDK**: The Canon SDK files must be properly placed in the project directory.

### Windows Specifics

- **OS**: Windows 10/11.
- **Compiler**: GCC (e.g., via [Scoop](https://scoop.sh/): `scoop install mingw`) or MinGW-w64.
- **SDK**: `edsdk/EDSDK.dll` and `edsdk/EDSDK.lib`.

### macOS Specifics

- **OS**: macOS 10.15+.
- **Compiler**: Xcode Command Line Tools (`xcode-select --install`).
- **SDK**: `EDSDK.framework` placed in the `libs` directory.

---

## 🛠️ Getting Started

### 1. Environment Setup

Clone the repository and ensure your `.env` file is configured (refer
to [ARCHITECTURE.md](./ARCHITECTURE.md#configuration--api) for all options).

```bash
# Example minimum .env
PORT=8080
CAPTURE_DIR=captures
```

### 2. Add Filters (optional)

Place any Adobe `.cube` LUT files into the `filters/` directory. Two presets are included:

| File | Effect |
|---|---|
| `filters/identity.cube` | Pass-through (no change) |
| `filters/warm.cube` | Warm tone — boosted reds, lifted shadows, reduced blues |

The engine hot-reloads every 5 seconds, so you can add or remove filter files while the server is running.

### 3. Build

#### Windows

```powershell
$env:CGO_ENABLED="1"
$env:GOARCH="amd64"
go build -o server.exe ./cmd/server/...
```

#### macOS

```bash
CGO_ENABLED=1 go build -o server ./cmd/server/...
```

### 4. Run

```powershell
./server.exe
```

---

## 🌐 Web Interface

The bundled web UI at [http://localhost:8080/](http://localhost:8080/) provides:

- **Live camera preview** via MJPEG.
- **Filter picker** — a chip-style selector that lists all loaded `.cube` presets; selecting one updates the preview and all subsequent captures instantly.
- **Photobooth flow** — 3-shot countdown with automatic strip generation.
- **Single capture** mode.
- **Strip download** as JPEG.

Embed the live preview in any frontend:

```html
<img src="http://localhost:8080/preview" alt="Camera Feed">
```

---

## 📡 API Reference

| Endpoint   | Method | Description                                            |
|------------|--------|--------------------------------------------------------|
| `/capture` | POST   | Trigger capture. Returns `original_url` (+ `filtered_url` if a filter is active). |
| `/preview` | GET    | MJPEG live-view stream. Frames are filtered in real-time if a filter is active. |
| `/filters` | GET    | List available filter names (loaded from `filters/*.cube`). |
| `/filter`  | PUT    | Set or clear the active filter (`{"name": "warm"}` or `{"name": ""}`). |

For full request/response schemas see the [OpenAPI 3.0 specification](./openapi.yaml).

---

## 📘 Documentation

- **[Architecture Guide](./ARCHITECTURE.md)**: Deep dive into system design, the filter pipeline, sequence flows, and EDSDK technicalities.
- **[API Specification](./openapi.yaml)**: Full OpenAPI 3.0 YAML with all request/response schemas.

---

## 🤝 Contributing

This project is focused on robustness and reliable hardware interaction. If you're contributing, please ensure you have
access to a supported Canon camera for testing.

---

## ⚖️ License

*Insert your license information here.*
