#!/usr/bin/env python3
"""Write the panel-mipi-dbi init-sequence firmware for the MF800 TFT.

Format (drivers/gpu/drm/tiny/panel-mipi-dbi.c): "MIPI DBI" + 7 zero bytes + version 1,
then commands as <cmd> <nparams> <params...>; cmd 0x00 with one param = delay in ms.
The driver sets the column/row window and sends RAMWR itself, so none are listed here.

Sequence: the stock GC9107 table (as decoded from the MF800H's vendor firmware,
github.com/ChokunPlayZ/openstick-lcd), minus its fixed 128x128 window.
Usage: mkfw.py OUT [--madctl 0xNN]
"""
import argparse

GC9107 = [
    (0xFE, []), (0xEF, []),                     # inter-register enable 1/2
    (0xB0, [0xC0]), (0xB2, [0x2F]), (0xB3, [0x03]), (0xB6, [0x19]), (0xB7, [0x01]),
    (0xAC, [0xC8]), (0xAB, [0x07]), (0xB4, [0x04]), (0xA8, [0x0C]),
    (0x3A, [0x05]),                             # COLMOD: 16 bit/pixel
    (0xB8, [0x08]), (0xE8, [0x20]), (0xE9, [0x44]), (0xEA, [0x44]), (0xEB, [0xB0]),
    (0xC6, [0x3D]), (0xC7, [0x10]),
    (0xF0, [0x0F, 0x20, 0x25, 0x35, 0x26, 0x3F, 0x30, 0x60, 0x26, 0x03, 0x00, 0x09, 0x13, 0x1A]),  # gamma +
    (0xF1, [0x00, 0x28, 0x7F, 0x39, 0xA5, 0x0F, 0x30, 0x60, 0x00, 0x05, 0x01, 0x0F, 0x0F, 0x1F]),  # gamma -
    (0xF4, [0x00, 0x00, 0xFF]),
]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("out")
    ap.add_argument("--madctl", type=lambda s: int(s, 0), default=None,
                    help="MADCTL (0x36) value: rotation/mirroring/BGR")
    a = ap.parse_args()
    seq = list(GC9107)
    if a.madctl is not None:
        seq.append((0x36, [a.madctl]))
    seq += [(0x11, []), ("delay", 120), (0x29, []), ("delay", 50)]  # sleep out, display on
    b = bytearray(b"MIPI DBI" + bytes(7) + bytes([1]))
    for cmd, p in seq:
        if cmd == "delay":
            b += bytes([0x00, 1, p])
        else:
            b += bytes([cmd, len(p), *p])
    with open(a.out, "wb") as f:
        f.write(b)


if __name__ == "__main__":
    main()
