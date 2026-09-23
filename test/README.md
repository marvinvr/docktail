# Test Containers

This directory contains test nginx containers for demonstrating DockTail functionality.

## Contents

- **nginx-web1/** - Test nginx instance 1 (exposed as `svc:web1` on port 443)
- **nginx-web2/** - Test nginx instance 2 (exposed as `svc:web2` on port 8443)

## Configuration

Both containers:
- Accept ANY hostname (configured with `server_name _`)
- Display instance information and container details
- Generate hostname files on startup
- Serve on container port 80

## Usage

These are referenced in the root `docker-compose.dev.yaml` and serve as examples for:
- Port publishing requirements
- Label configuration
- Multi-instance service setup
