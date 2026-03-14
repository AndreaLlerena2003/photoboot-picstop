[![Go Version](https://img.shields.io/github/go-mod/go-version/AndreaLlerena2003/photoboot-picstop)](https://golang.org)
[![Platform: Windows](https://img.shields.io/badge/platform-windows-blue)](https://www.microsoft.com/windows)
[![Platform: macOS](https://img.shields.io/badge/platform-macOS-lightgrey)](https://www.apple.com/macos)

A high-performance backend service for Canon camera control, designed for photobooth applications. This service provides
a RESTful API to capture photos and a real-time MJPEG stream for live preview, using the **Canon EDSDK**.

---

## 🚀 Features

- **📸 Instant Capture**: Trigger photo capture via HTTP POST with configurable timeouts.
- **🎥 Live Preview**: High-speed MJPEG stream (Electronic Viewfinder - EVF) compatible with standard `<img>` tags.
- **🔄 Auto-Reconnection**: Seamlessly handles camera disconnections and reconnections without service restarts.
- **🏗️ Clean Architecture**: Decoupled domain logic and infrastructure, making the codebase maintainable and testable.
- **⚡ Performance-First**: Uses optimized CGo bindings for direct communication with the Canon EDSDK.

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

### 2. Build

#### Windows

```powershell
# Set CGo environment variables
$env:CGO_ENABLED="1"
$env:GOARCH="amd64"

# Build the server
go build -o server.exe ./cmd/server/...
```

#### macOS

```bash
# Build the server
CGO_ENABLED=1 go build -o server ./cmd/server/...
```

### 3. Run

```powershell
./server.exe
```

---

## 📡 API Reference

The server exposes two primary endpoints:

| Endpoint   | Method | Description                                      |
|------------|--------|--------------------------------------------------|
| `/capture` | `POST` | Triggers a photo capture. Returns JSON metadata. |
| `/preview` | `GET`  | Starts an MJPEG live view stream.                |

For detailed API specifications, view the [OpenAPI 3.0 Documentation](./openapi.yaml).

---

## 📘 Documentation

- **[Architecture Guide](./ARCHITECTURE.md)**: Deep dive into the system design, sequence flows, and EDSDK
  technicalities.
- **[API Specification](./openapi.yaml)**: Full OpenAPI 3.0 YAML with all request/response schemas.

---

## 🤝 Contributing

This project is focused on robustness and reliable hardware interaction. If you're contributing, please ensure you have
access to a supported Canon camera for testing.

---

## ⚖️ License

*Insert your license information here.*
