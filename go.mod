module docker-systemd

go 1.22

// Pinned so CI cannot silently build with a different toolchain (defect F11).
toolchain go1.22.5

require golang.org/x/sys v0.22.0
