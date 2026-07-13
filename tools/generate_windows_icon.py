#!/usr/bin/env python3
"""Generate VS AI Proxy Windows icon assets.

The icon is intentionally generated from vector-like drawing commands so the
brand asset is reproducible and does not depend on an external design file.
"""
from __future__ import annotations

from pathlib import Path
from PIL import Image, ImageDraw, ImageFilter, ImageFont

ROOT = Path(__file__).resolve().parents[1]
OUT_DIR = ROOT / "assets" / "brand"
PNG_PATH = OUT_DIR / "vs-ai-proxy-icon.png"
ICO_PATH = OUT_DIR / "vs-ai-proxy.ico"
SIZES = [16, 24, 32, 48, 64, 128, 256]


def rounded_rectangle(draw: ImageDraw.ImageDraw, box, radius, fill, outline=None, width=1):
    draw.rounded_rectangle(box, radius=radius, fill=fill, outline=outline, width=width)


def draw_icon(size: int) -> Image.Image:
    scale = size / 256
    img = Image.new("RGBA", (size, size), (0, 0, 0, 0))

    # Soft shadow
    shadow = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    sd = ImageDraw.Draw(shadow)
    rounded_rectangle(sd, [22*scale, 26*scale, 234*scale, 238*scale], 46*scale, (0, 0, 0, 120))
    shadow = shadow.filter(ImageFilter.GaussianBlur(max(1, int(8 * scale))))
    img.alpha_composite(shadow)

    draw = ImageDraw.Draw(img)

    # Base app tile: Visual Studio-like purple, but not copying official marks.
    rounded_rectangle(draw, [20*scale, 18*scale, 236*scale, 234*scale], 46*scale, (92, 44, 180, 255))
    rounded_rectangle(draw, [28*scale, 26*scale, 228*scale, 226*scale], 38*scale, (47, 57, 171, 255))

    # Diagonal proxy gradient panels.
    poly1 = [(38*scale, 178*scale), (98*scale, 72*scale), (160*scale, 104*scale), (96*scale, 210*scale)]
    poly2 = [(96*scale, 72*scale), (184*scale, 42*scale), (218*scale, 90*scale), (138*scale, 124*scale)]
    poly3 = [(120*scale, 136*scale), (214*scale, 96*scale), (220*scale, 178*scale), (142*scale, 212*scale)]
    draw.polygon(poly1, fill=(128, 85, 255, 255))
    draw.polygon(poly2, fill=(0, 212, 255, 230))
    draw.polygon(poly3, fill=(52, 235, 171, 220))

    # Central AI/proxy hub.
    hub = (128*scale, 132*scale)
    nodes = [(74*scale, 92*scale), (182*scale, 78*scale), (190*scale, 178*scale), (76*scale, 184*scale)]
    for node in nodes:
        draw.line([hub, node], fill=(238, 246, 255, 185), width=max(2, int(7*scale)))
    for x, y in nodes:
        r = 13 * scale
        draw.ellipse([x-r, y-r, x+r, y+r], fill=(246, 250, 255, 255), outline=(49, 196, 255, 255), width=max(1, int(3*scale)))
    r = 24 * scale
    draw.ellipse([hub[0]-r, hub[1]-r, hub[0]+r, hub[1]+r], fill=(15, 23, 42, 245), outline=(255, 255, 255, 235), width=max(2, int(5*scale)))

    # Compact "AI" mark, readable on large icons and harmless on small icons.
    if size >= 48:
        try:
            font = ImageFont.truetype("Arial Bold.ttf", max(14, int(35*scale)))
        except Exception:
            font = ImageFont.load_default()
        text = "AI"
        bbox = draw.textbbox((0, 0), text, font=font)
        tx = hub[0] - (bbox[2] - bbox[0]) / 2
        ty = hub[1] - (bbox[3] - bbox[1]) / 2 - 1*scale
        draw.text((tx, ty), text, font=font, fill=(255, 255, 255, 255))

    # Forward arrow for proxy/routing identity.
    arrow = [(152*scale, 206*scale), (206*scale, 206*scale), (206*scale, 188*scale), (232*scale, 216*scale), (206*scale, 244*scale), (206*scale, 226*scale), (152*scale, 226*scale)]
    draw.polygon(arrow, fill=(255, 255, 255, 238))

    # Subtle top highlight.
    draw.arc([34*scale, 30*scale, 224*scale, 222*scale], 205, 310, fill=(255, 255, 255, 70), width=max(1, int(5*scale)))

    return img


def main() -> None:
    OUT_DIR.mkdir(parents=True, exist_ok=True)
    base = draw_icon(256)
    base.save(PNG_PATH)
    icons = [draw_icon(size) for size in SIZES]
    icons[-1].save(ICO_PATH, sizes=[(s, s) for s in SIZES], append_images=icons[:-1])
    print(f"generated {PNG_PATH}")
    print(f"generated {ICO_PATH}")


if __name__ == "__main__":
    main()
