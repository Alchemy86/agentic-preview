#!/usr/bin/env python3
"""Draw agentic-preview's mark, with Glyphsmith.

No hand-drawn artwork and no embedded font: the wordmark is set in Glyphsmith's
house alphabet and composed by :class:`glyphsmith.mark.Mark`, through the same
public API any other project's mark goes through.  The only thing this file adds
is the one thing a mark is allowed to add -- its own motif and its own accent.

    pip install --user git+https://github.com/Alchemy86/Glyphsmith
    python3 brand/make.py

Deterministic: re-running it reproduces both SVGs byte for byte.
"""

import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

from motif import LaneBand  # noqa: E402

from glyphsmith.mark import Mark  # noqa: E402

MARK = Mark(
    word="AGENTICPREVIEW",
    tagline="ONE HEADER, THE RIGHT POD",
    accent="phosphor",           # terminal phosphor green
    motif=LaneBand(),
    # Fourteen letters want the family's wide canvas, the one AsciiWorldEngine
    # and ShowReel settle at, rather than a smaller scale on the 1200 panel.
    width=1400,
    height=420,
    scale=1.10,
    cap_y=84,
    band_y=LaneBand.band_y,
    tagline_y=316,
    attribution="agentic-preview",
    regen="python3 brand/make.py",
    logo_comment="agentic-preview logo — the wordmark, the full stop, the lane band",
    icon_comment="agentic-preview icon — three lanes, the middle one diverted",
)


def main():
    for name, content in (("agentic-preview-logo.svg", MARK.logo()),
                          ("agentic-preview-icon.svg", MARK.icon())):
        path = os.path.join(HERE, name)
        with open(path, "w") as f:
            f.write(content)
        print(f"wrote brand/{name} ({len(content)} bytes)")


if __name__ == "__main__":
    main()
