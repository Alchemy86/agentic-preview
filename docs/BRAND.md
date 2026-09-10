# The mark

`brand/make.py` is the source of truth. It sets the wordmark in
[Glyphsmith](https://github.com/Alchemy86/Glyphsmith)'s house alphabet — no embedded font,
no traced outlines — and writes all three SVGs; re-running it reproduces them byte for byte.
Never hand-edit the SVGs.

```bash
pip install --user git+https://github.com/Alchemy86/Glyphsmith
python3 brand/make.py
```

`make.py` writes the logo, the icon, and `agentic-preview-social.svg` — the same mark
composed on the 1280×640 panel GitHub renders a repository's social preview card in, which
is what a link to this repo unfurls to in Slack or Teams.

## The PNGs are derived

The three PNGs are **derived artifacts**, committed only because the places they are used
do not accept an SVG: GitHub's issue bodies, its social preview setting, and the `icon`
field of the Helm chart, which is what Artifact Hub renders on the package page.
Regenerate them whenever the SVGs change:

```bash
magick -background none brand/agentic-preview-logo.svg \
  -resize 1400x -depth 8 -strip brand/agentic-preview-logo.png

magick -background none brand/agentic-preview-icon.svg \
  -resize 512x512 -depth 8 -strip brand/agentic-preview-icon.png

magick brand/agentic-preview-social.svg -background '#0d1117' -flatten \
  -alpha remove -alpha off -resize 1280x640 -depth 8 -strip \
  brand/agentic-preview-social.png
```

The social card is flattened onto the panel colour rather than left transparent: GitHub
asks for a solid background, because the card is rendered against whatever the reading
client's theme happens to be. Setting it is a web-UI action — Settings → Social preview →
Edit → *Upload an image…* — there is no API for it.
