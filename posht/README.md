# posht

Interactive terminal-capability test for posh — a "diff on a POSH".
Design and scope: [`docs/posht.md`](../docs/posht.md).

A static Go TUI (Bubble Tea) that walks through the terminal features the
posh stack claims to support — colors, attributes, wide chars, wrap,
scroll regions, mouse, paste, OSC title/clipboard/hyperlinks, kitty
graphics — rendering each one and asking you to confirm what you see.
It opens with a checklist you can deselect from, records pass/fail/skip
per feature, and writes a JSON receipt on exit (non-zero status if
anything failed). By default the receipt lands in
`~/.local/log/posht/<datetime>-<terminal>.json` and posht prints that
path; `--json -` puts the JSON on stdout instead.

```sh
nix build .#posht   # hermetic build → ./result-posht/bin/posht
go build .          # fast dev-loop; needs Go ≥ 1.25 (the toolchain auto-fetches)
./posht             # local baseline run; prints the receipt path on exit
./posht --list      # test ids, for --only / --skip
./posht --json -    # JSON receipt to stdout
./posht -o report.md # also write the markdown report to a file
```

Run it three ways and diff the reports: directly in your terminal
(baseline), inside a local posh session (emulator + replay), and on a
remote over posh (the full pipeline):

```sh
./run-remote.sh box           # build for box's arch, scp, run via posh ssh
./run-remote.sh box -o /tmp/posht-report.md
```

A feature that passes the baseline but fails under posh is a posh bug.
Tests that overlap known posh gaps (#27 BEL/OSC 52 forwarding, #29 kitty
graphics over remote) say so on screen.
