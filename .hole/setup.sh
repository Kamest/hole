#!/bin/bash

set -euo pipefail

# Define the Go version and architecture
GO_VERSION="1.25.12"
ARCH="arm64" # Change to arm64 if you are on an ARM-based machine (like Apple Silicon)

# Define userspace paths
INSTALL_DIR="$HOME/.local"
GO_DIR="$INSTALL_DIR/go"
TAR_FILE="$HOME/go.tar.gz"

echo "Downloading Go ${GO_VERSION} for ${ARCH}..."
curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -o "$TAR_FILE"

echo "Creating installation directory..."
mkdir -p "$INSTALL_DIR"

echo "Extracting Go to ${GO_DIR}..."
rm -rf "$GO_DIR" # Remove old userspace install if it exists
tar -C "$INSTALL_DIR" -xzf "$TAR_FILE"
rm "$TAR_FILE"

echo "Configuring environment variables for interactive and non-interactive shells..."

GO_PATH_EXPORT='export PATH="$HOME/.local/go/bin:$HOME/go/bin:$PATH"'

# 1. Add to ~/.profile (Used by login shells)
if ! grep -q "$HOME/.local/go/bin" "$HOME/.profile" 2>/dev/null; then
    echo "$GO_PATH_EXPORT" >> "$HOME/.profile"
fi

# 2. Prepend to the VERY TOP of ~/.bashrc (Before the non-interactive early exit)
if ! grep -q "$HOME/.local/go/bin" "$HOME/.bashrc" 2>/dev/null; then
    # Read existing .bashrc, prepend the export, and write it back
    echo -e "$GO_PATH_EXPORT\n$(cat "$HOME/.bashrc" 2>/dev/null)" > "$HOME/.bashrc"
fi

# 3. Append to your existing ~/.bash_env (For non-interactive shells)
if ! grep -q "$HOME/.local/go/bin" "$HOME/.bash_env" 2>/dev/null; then
    echo "$GO_PATH_EXPORT" >> "$HOME/.bash_env"
fi

# golangci-lint — the same version CI pins (.github/workflows/ci.yml, golangci-lint-action
# `version:`); keep the two in sync, or `make lint` here and the CI gate disagree.
GOLANGCI_LINT_VERSION="v2.5.0"
echo "➡️ Installing golangci-lint ${GOLANGCI_LINT_VERSION}..."
curl -fsSL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
  | sh -s -- -b "$HOME/go/bin" "${GOLANGCI_LINT_VERSION}"

if ! "$HOME/go/bin/golangci-lint" version >/dev/null 2>&1; then
  echo "ERROR: golangci-lint was installed but does not run" >&2
  exit 1
fi
