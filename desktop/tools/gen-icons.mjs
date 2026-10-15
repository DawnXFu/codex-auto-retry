// Generates the app icon set (pure Node, no deps):
//   desktop/build/appicon.png        — 512px PNG, wails-build convention
//   desktop/build/windows/icon.ico   — multi-size ICO (PNG-compressed entries)
//
// Mark: dark rounded tile (sidebar #14161b) + green retry arc (brand #16a34a),
// arrowhead pointing clockwise = auto-resume. Supersampled 4x for AA.
// Regenerate: node desktop/tools/gen-icons.mjs

import { deflateSync } from "node:zlib";
import { writeFileSync, mkdirSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const BUILD = join(dirname(fileURLToPath(import.meta.url)), "..", "build");

const TILE = [0x14, 0x16, 0x1b];
const MARK = [0x16, 0xa3, 0x4a];

// ---- geometry (unit square, y down, angles clockwise from +x) ----
const R_TILE = 0.2; // corner radius
const R_ARC = 0.26; // arc centerline radius
const T_ARC = 0.13; // arc stroke width
const GAP = [290, 345]; // degrees; gap sits upper-right
const HEAD_LEN = 0.11;
const HEAD_HALF = 0.1;

const rad = (d) => (d * Math.PI) / 180;
const D2R = Math.PI / 180;

function inRoundRect(x, y) {
  const r = R_TILE;
  if (x < 0 || x > 1 || y < 0 || y > 1) return false;
  const cx = x < 0.5 ? r : 1 - r;
  const cy = y < 0.5 ? r : 1 - r;
  if (x >= r && x <= 1 - r) return true;
  if (y >= r && y <= 1 - r) return true;
  return (x - cx) ** 2 + (y - cy) ** 2 <= r * r;
}

function angleDeg(dx, dy) {
  let a = Math.atan2(dy, dx) / D2R;
  return a < 0 ? a + 360 : a;
}

function onArc(x, y) {
  const dx = x - 0.5;
  const dy = y - 0.5;
  const d = Math.hypot(dx, dy);
  if (Math.abs(d - R_ARC) > T_ARC / 2) return false;
  const a = angleDeg(dx, dy);
  return a >= GAP[1] || a <= GAP[0];
}

// Arrowhead: apex extends from the arc end (at GAP[0]) along the clockwise
// tangent into the gap.
const e = { x: 0.5 + R_ARC * Math.cos(rad(GAP[0])), y: 0.5 + R_ARC * Math.sin(rad(GAP[0])) };
const tan = { x: Math.cos(rad(GAP[0] + 90)), y: Math.sin(rad(GAP[0] + 90)) };
const nrm = { x: -tan.y, y: tan.x };
const apex = { x: e.x + tan.x * HEAD_LEN, y: e.y + tan.y * HEAD_LEN };
const b1 = { x: e.x + nrm.x * HEAD_HALF, y: e.y + nrm.y * HEAD_HALF };
const b2 = { x: e.x - nrm.x * HEAD_HALF, y: e.y - nrm.y * HEAD_HALF };

function inTri(x, y) {
  const s = (p, q) => (x - q.x) * (p.y - q.y) - (p.x - q.x) * (y - q.y);
  const d1 = s(apex, b1);
  const d2 = s(b1, b2);
  const d3 = s(b2, apex);
  const neg = d1 < 0 || d2 < 0 || d3 < 0;
  const pos = d1 > 0 || d2 > 0 || d3 > 0;
  return !(neg && pos);
}

const SS = 4; // supersample factor
function render(size) {
  const px = new Uint8Array(size * size * 4);
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      let tile = 0;
      let mark = 0;
      for (let sy = 0; sy < SS; sy++) {
        for (let sx = 0; sx < SS; sx++) {
          const u = (x + (sx + 0.5) / SS) / size;
          const v = (y + (sy + 0.5) / SS) / size;
          if (inRoundRect(u, v)) tile++;
          if (onArc(u, v) || inTri(u, v)) mark++;
        }
      }
      const i = (y * size + x) * 4;
      const ta = tile / (SS * SS);
      const ma = mark / (SS * SS);
      // mark over tile over transparent
      const a = ma + ta * (1 - ma);
      for (let c = 0; c < 3; c++) {
        px[i + c] = a === 0 ? 0 : Math.round((MARK[c] * ma + TILE[c] * ta * (1 - ma)) / a);
      }
      px[i + 3] = Math.round(a * 255);
    }
  }
  return px;
}

// ---- PNG encoder ----
const crcTable = new Int32Array(256).map((_, n) => {
  let c = n;
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  return c;
});
function crc32(buf) {
  let c = -1;
  for (const b of buf) c = crcTable[(c ^ b) & 0xff] ^ (c >>> 8);
  return (c ^ -1) >>> 0;
}
function chunk(type, data) {
  const out = Buffer.alloc(12 + data.length);
  out.writeUInt32BE(data.length, 0);
  out.write(type, 4, "ascii");
  data.copy(out, 8);
  out.writeUInt32BE(crc32(out.subarray(4, 8 + data.length)), 8 + data.length);
  return out;
}
function png(size, rgba) {
  const raw = Buffer.alloc(size * (size * 4 + 1));
  for (let y = 0; y < size; y++) {
    raw[y * (size * 4 + 1)] = 0;
    Buffer.from(rgba.buffer, y * size * 4, size * 4).copy(raw, y * (size * 4 + 1) + 1);
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(size, 0);
  ihdr.writeUInt32BE(size, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 6; // RGBA
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk("IHDR", ihdr),
    chunk("IDAT", deflateSync(raw, { level: 9 })),
    chunk("IEND", Buffer.alloc(0)),
  ]);
}

// ---- ICO writer (PNG-compressed entries) ----
function ico(images) {
  const header = Buffer.alloc(6);
  header.writeUInt16LE(0, 0);
  header.writeUInt16LE(1, 2);
  header.writeUInt16LE(images.length, 4);
  const dir = Buffer.alloc(16 * images.length);
  let off = 6 + dir.length;
  images.forEach(([size, data], i) => {
    dir[i * 16] = size >= 256 ? 0 : size;
    dir[i * 16 + 1] = size >= 256 ? 0 : size;
    dir[i * 16 + 2] = 0; // palette
    dir[i * 16 + 3] = 0;
    dir.writeUInt16LE(1, i * 16 + 4); // planes
    dir.writeUInt16LE(32, i * 16 + 6); // bpp
    dir.writeUInt32LE(data.length, i * 16 + 8);
    dir.writeUInt32LE(off, i * 16 + 12);
    off += data.length;
  });
  return Buffer.concat([header, dir, ...images.map(([, d]) => d)]);
}

mkdirSync(join(BUILD, "windows"), { recursive: true });

const sizes = [16, 24, 32, 48, 64, 128, 256];
const entries = sizes.map((s) => [s, png(s, render(s))]);
writeFileSync(join(BUILD, "windows", "icon.ico"), ico(entries));
writeFileSync(join(BUILD, "appicon.png"), png(512, render(512)));
console.log("wrote", join(BUILD, "appicon.png"), "and", join(BUILD, "windows", "icon.ico"));
