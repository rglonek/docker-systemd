# Fixture unit files

Unit files harvested from the supported base images, used by the L1 parser
tests (designs/docs/next/11-testing.md §2). They are verbatim copies, so a
parser change that breaks a real distro unit fails the build.

Add a file here whenever a base image ships a unit whose shape is not already
covered — a new `Type=`, a new directive, an unusual `ExecStart=` quoting — and
the table-driven test in `internal/unitfile` will pick it up automatically.
