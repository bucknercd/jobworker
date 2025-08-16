#!/usr/bin/env bash
set -euo pipefail

JOBROOT="/opt/jobroot"

usage() {
  echo "Usage: $0 [build|clean|nuke]"
  exit 1
}

action="${1:-build}"

unmount_proc() {
  if findmnt -rno TARGET "$JOBROOT/proc" >/dev/null 2>&1; then
    echo "    [-] Unmounting $JOBROOT/proc"
    # Try normal unmount first; if busy, lazy unmount
    if ! sudo umount "$JOBROOT/proc" 2>/dev/null; then
      echo "       (mount busy) Lazy unmounting $JOBROOT/proc"
      sudo umount -l "$JOBROOT/proc"
    fi
  fi
}

case "$action" in
  build)
    echo "[*] (Re)building minimal chroot at $JOBROOT"
    unmount_proc

    if [ -d "$JOBROOT" ]; then
      echo "    [-] Removing existing $JOBROOT"
      sudo rm -rf "$JOBROOT"
    fi

    # Minimal commands to include
    BINARIES=(/bin/sh /bin/ls /bin/cat /bin/echo)
    EXTRA_BINS=(/usr/sbin/nologin)

    # Base dirs
    sudo mkdir -p "$JOBROOT"/{bin,lib,lib64,usr/bin,usr/lib,usr/lib64,usr/sbin,etc,dev,proc,root}

    copy_with_libs() {
      local bin="$1"
      echo "    [+] Installing $bin"
      sudo install -D "$bin" "$JOBROOT$bin"
      ldd "$bin" | awk '{
        if (match($3, "/")) { print $3 }
        else if (match($1, "/")) { print $1 }
      }' | sort -u | while read -r dep; do
        if [ -n "$dep" ] && [ ! -e "$JOBROOT$dep" ]; then
          echo "       -> $dep"
          sudo install -D "$dep" "$JOBROOT$dep"
        fi
      done
    }

    for bin in "${BINARIES[@]}"; do copy_with_libs "$bin"; done
    for bin in "${EXTRA_BINS[@]}"; do
      if command -v "$bin" >/dev/null 2>&1; then copy_with_libs "$bin"; else
        echo "    [!] Skipping $bin — not found on host"
      fi
    done

    # /dev/null (needs root)
    sudo mknod -m 666 "$JOBROOT/dev/null" c 1 3 || true

    # Minimal passwd & group
    sudo tee "$JOBROOT/etc/passwd" >/dev/null <<EOF
root:x:0:0:root:/root:/bin/sh
nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin
EOF
    sudo tee "$JOBROOT/etc/group" >/dev/null <<EOF
root:x:0:
nobody:x:65534:
EOF

    echo "[*] Minimal chroot environment setup complete."
    echo "    To use: sudo mount -t proc proc $JOBROOT/proc && sudo chroot $JOBROOT /bin/sh"
    ;;

  clean)
    echo "[*] Cleaning chroot at $JOBROOT"
    unmount_proc
    if [ -d "$JOBROOT" ]; then
      echo "    [-] Removing $JOBROOT"
      sudo rm -rf "$JOBROOT"
    fi
    echo "[+] Clean complete."
    ;;

  nuke)
    echo "[*] FORCE cleaning chroot at $JOBROOT"
    # Always lazy-umount in nuke
    if findmnt -rno TARGET "$JOBROOT/proc" >/dev/null 2>&1; then
      echo "    [-] Lazy unmounting $JOBROOT/proc"
      sudo umount -l "$JOBROOT/proc" || true
    fi
    sudo rm -rf "$JOBROOT" || true
    echo "[+] Nuke complete."
    ;;

  *) usage ;;
esac

