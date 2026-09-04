#!/usr/bin/env python3
"""Rebuild internal/api/ui/mesh/brain.bin.gz from BodyParts3D.

Requires: python3 with numpy, trimesh, fast_simplification
(pip install --user trimesh fast_simplification) and network access.

Output format (little endian): uint32 vertex count, uint32 face count,
float32 xyz per vertex, uint32 three indices per face. The mesh is centred
on the origin and scaled so its longest axis spans 2 units; the axes are
BodyParts3D's (x left-right, y back-front, z up).
"""
import gzip
import os
import struct
import sys
import urllib.request

import fast_simplification
import numpy as np
import trimesh

HERE = os.path.dirname(os.path.abspath(__file__))
MIRROR = "https://raw.githubusercontent.com/Kevin-Mattheus-Moerman/BodyParts3D/main/assets/BodyParts3D_data/stl/"
OUT = os.path.join(HERE, "..", "..", "internal", "api", "ui", "mesh", "brain.bin.gz")
TARGET_FACES = 45000


def main():
    cache = os.path.join(HERE, "cache")
    os.makedirs(cache, exist_ok=True)
    parts = []
    for line in open(os.path.join(HERE, "part_ids.txt")):
        fma = line.strip()
        if not fma:
            continue
        path = os.path.join(cache, fma + ".stl")
        if not os.path.exists(path):
            print("fetch", fma, file=sys.stderr)
            urllib.request.urlretrieve(MIRROR + fma + ".stl", path)
        parts.append(trimesh.load(path, force="mesh"))
    m = trimesh.util.concatenate(parts)
    m.merge_vertices()
    v, f = fast_simplification.simplify(
        m.vertices.astype(np.float32), m.faces.astype(np.int64),
        target_reduction=1 - TARGET_FACES / len(m.faces))
    s = trimesh.Trimesh(v, f, process=True)
    s.apply_translation(-s.bounds.mean(axis=0))
    s.apply_scale(2.0 / max(s.extents))
    raw = struct.pack("<II", len(s.vertices), len(s.faces))
    raw += s.vertices.astype("<f4").tobytes() + s.faces.astype("<u4").tobytes()
    with gzip.open(OUT, "wb", compresslevel=9) as fh:
        fh.write(raw)
    print(f"{len(parts)} parts, {len(s.faces)} faces, {len(s.vertices)} vertices, {os.path.getsize(OUT)} bytes gz")


if __name__ == "__main__":
    main()
