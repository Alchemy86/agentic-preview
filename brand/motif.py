"""agentic-preview's own motif: the lane band, and the diverted lane.

The motif is what the project is.  AtlasGB puts a region bar under its wordmark
because the claim it makes is "the whole space, accounted for"; agentic-preview
puts a *row of lanes with one of them picked out*, because that is the whole
product in one shape -- the same row of services you always had, one of them
diverted by a header.

Five lanes are drawn in the muted rule colour and the second in the accent.  The
contrast is the meaning: an all-accent row would say "everything is a preview",
and a row with nothing picked out would say nothing at all.

Drawn through the public API on the canvas units :class:`glyphsmith.mark.Mark`
hands a band, exactly as ``RegionBar`` in Glyphsmith's own gallery is.  Nothing
here reaches into Glyphsmith that a project bringing its own motif could not.
"""

from glyphsmith.motifs import Motif


class LaneBand(Motif):
    """A row of six equal lanes, the second one diverted.

    Equal widths and even gaps on purpose, unlike AtlasGB's region bar whose
    segments are proportional to the RAM they stand for: here the lanes are
    peer services, and no service is bigger than another.  Which one is picked
    out is arbitrary, so it is the second rather than the first -- a diverted
    lane at the head of the row reads as a label for the row behind it.
    """

    lanes = 6
    diverted = 1            # zero-based: the SECOND lane
    lane_gap = 12
    band_height = 14
    band_y = 284            # the tall-canvas band line the 1400x420 marks use

    #: The muted lanes are the accent held back to the family's rule weight --
    #: the same device Glyphsmith's metrics comb uses for its rule and
    #: ShowReel's timeline for the scenes it is not highlighting.  One hue, two
    #: weights, so the row reads as one row.
    rule_opacity = 0.42

    def band(self, lay, palette):
        inner = lay.total - self.lane_gap * (self.lanes - 1)
        w = inner / self.lanes
        parts, cx = [], lay.x0
        for i in range(self.lanes):
            live = i == self.diverted
            fill = (f'fill="{palette.accent}"' if live else
                    f'fill="{palette.accent}" '
                    f'fill-opacity="{self.rule_opacity:.2f}"')
            parts.append(f'<rect x="{cx:.1f}" y="{self.band_y:.1f}" '
                         f'width="{w:.1f}" height="{self.band_height}" '
                         f'rx="{self.band_height / 2:.1f}" {fill}/>')
            cx += w + self.lane_gap
        return "\n".join(parts)

    def icon(self, box, palette):
        # Multiples of 8 inside the 128 box, so at 16 px the three rows stay
        # three rows instead of blurring into one -- the same reason AtlasGB's
        # region bar keeps its icon on the same grid.
        #
        # 3 lanes of 16 with 8 between them is 64, centred at y = 32/56/80.
        rows, h, gap = 3, 16, 8
        top = (box - (rows * h + (rows - 1) * gap)) // 2
        parts = []
        for i in range(rows):
            live = i == rows // 2          # the middle lane
            fill = (f'fill="{palette.accent}"' if live else
                    f'fill="{palette.accent}" '
                    f'fill-opacity="{self.rule_opacity:.2f}"')
            parts.append(f'<rect x="24" y="{top + i * (h + gap)}" width="80" '
                         f'height="{h}" rx="{h // 2}" {fill}/>')
        return "\n".join(parts)
