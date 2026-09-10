#!/usr/bin/env python3
"""Draw agentic-preview's mark, with Glyphsmith.

No hand-drawn artwork and no embedded font: the wordmark is set in Glyphsmith's
house alphabet and composed by :class:`glyphsmith.mark.Mark`, through the same
public API any other project's mark goes through.  The only thing this file adds
is the one thing a mark is allowed to add -- its own motif and its own accent.

    pip install --user git+https://github.com/Alchemy86/Glyphsmith
    python3 brand/make.py

Deterministic: re-running it reproduces all three SVGs byte for byte.
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


# The same mark on the 2:1 panel GitHub's social preview card is displayed in.
# Not a crop of the logo: a crop either loses the lane band or shrinks the
# wordmark to nothing, and the card is often rendered around 360 px wide. So it
# is composed at the card's own aspect, with the block centred in the taller
# panel -- cap_y, the band line and the tagline all shifted down by the same
# 105 px, which is what centres a 262 px block in 640.
#
# The card carries no description text. Slack, Teams and the rest render the
# repository's own title and description beside the image, so a line of type
# repeating them would be the same words twice.
SOCIAL_SHIFT = 105

_social_motif = LaneBand()
_social_motif.band_y = LaneBand.band_y + SOCIAL_SHIFT

SOCIAL = Mark(
    word="AGENTICPREVIEW",
    tagline="ONE HEADER, THE RIGHT POD",
    accent="phosphor",
    motif=_social_motif,
    # 1280x640 is what GitHub asks for. At scale 1.10 the fourteen letters and
    # the full stop come to 1095.6 px, leaving a 92 px margin either side.
    width=1280,
    height=640,
    scale=1.10,
    cap_y=84 + SOCIAL_SHIFT,
    band_y=_social_motif.band_y,
    tagline_y=316 + SOCIAL_SHIFT,
    attribution="agentic-preview",
    regen="python3 brand/make.py",
    logo_comment="agentic-preview social card - the mark on GitHub's 1280x640 preview panel",
    icon_comment="agentic-preview icon - three lanes, the middle one diverted",
)


def main():
    for name, content in (("agentic-preview-logo.svg", MARK.logo()),
                          ("agentic-preview-icon.svg", MARK.icon()),
                          ("agentic-preview-social.svg", SOCIAL.logo())):
        path = os.path.join(HERE, name)
        with open(path, "w") as f:
            f.write(content)
        print(f"wrote brand/{name} ({len(content)} bytes)")


if __name__ == "__main__":
    main()
