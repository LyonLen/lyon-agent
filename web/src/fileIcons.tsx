// fileIcons — VS Code explorer-style decorations for the files panel:
// a solid-colored page carrying the extension's short label, a folder
// glyph, and the git badge map (新增 green / 变更 blue / 删除 gray,
// conflicted orange) — the panel's whole decoration contract lives here.
import type { GitChange } from "./gen/lyon/v1/git_pb";

// ext → [fill, darkLabel?]. Hues follow the familiar Material-icon
// family; darkLabel marks bright fills that need near-black text.
const EXT_COLORS: Record<string, [string, boolean?]> = {
  go: ["#00add8"],
  ts: ["#3178c6"],
  tsx: ["#3178c6"],
  js: ["#f7df1e", true],
  mjs: ["#f7df1e", true],
  cjs: ["#f7df1e", true],
  jsx: ["#4a9fd8"],
  py: ["#3572a5"],
  rs: ["#dea584"],
  java: ["#b07219"],
  kt: ["#7f52ff"],
  c: ["#5f8fd6"],
  h: ["#5f8fd6"],
  cpp: ["#f34b7d"],
  cc: ["#f34b7d"],
  cxx: ["#f34b7d"],
  hpp: ["#f34b7d"],
  html: ["#e34c26"],
  htm: ["#e34c26"],
  css: ["#42a5f5"],
  scss: ["#cd6799"],
  less: ["#3869a3"],
  json: ["#cbcb41", true],
  yaml: ["#d4574e"],
  yml: ["#d4574e"],
  md: ["#519aba"],
  sh: ["#89e051", true],
  bash: ["#89e051", true],
  zsh: ["#89e051", true],
  fish: ["#89e051", true],
  svg: ["#ffb300", true],
  png: ["#a074c4"],
  jpg: ["#a074c4"],
  jpeg: ["#a074c4"],
  gif: ["#a074c4"],
  webp: ["#a074c4"],
  bmp: ["#a074c4"],
  ico: ["#a074c4"],
  pdf: ["#e2483d"],
  zip: ["#b8743c"],
  tar: ["#b8743c"],
  gz: ["#b8743c"],
  "7z": ["#b8743c"],
  rar: ["#b8743c"],
  exe: ["#9370db"],
  dll: ["#9370db"],
  bin: ["#9370db"],
  toml: ["#c47f4a"],
  xml: ["#e37933"],
  csv: ["#7ab648"],
  tsv: ["#7ab648"],
  txt: ["#8a8a8a"],
  log: ["#8a8a8a"],
  ini: ["#8a8a8a"],
  cfg: ["#8a8a8a"],
  conf: ["#8a8a8a"],
  lock: ["#8a8a8a"],
  vue: ["#41b883"],
  rb: ["#cc342d"],
  php: ["#777bb4"],
  sql: ["#e38c00"],
  swift: ["#f05138"],
  dart: ["#00b4ab"],
  proto: ["#5c6bc0"],
  pptx: ["#d24726"],
  ppt: ["#d24726"],
  xlsx: ["#217346"],
  xlsm: ["#217346"],
  xls: ["#217346"],
  docx: ["#2b579a"],
  doc: ["#2b579a"],
};

// Full-name overrides (lowercased): files whose identity is the name,
// not the extension → [fill, label].
const NAME_ICONS: Record<string, [string, string]> = {
  ".gitignore": ["#f14c28", "GIT"],
  ".gitattributes": ["#f14c28", "GIT"],
  ".gitmodules": ["#f14c28", "GIT"],
  license: ["#8a8a8a", "LIC"],
  licence: ["#8a8a8a", "LIC"],
  makefile: ["#4f9d4f", "MK"],
  gnumakefile: ["#4f9d4f", "MK"],
  "go.mod": ["#00add8", "GO"],
  "go.sum": ["#00add8", "GO"],
  "package.json": ["#6ba43a", "NPM"],
  "package-lock.json": ["#6ba43a", "NPM"],
  "cargo.toml": ["#dea584", "RS"],
  "cargo.lock": ["#dea584", "RS"],
  dockerfile: ["#2496ed", "DKR"],
  "docker-compose.yml": ["#2496ed", "DKR"],
  "docker-compose.yaml": ["#2496ed", "DKR"],
  ".env": ["#89e051", "ENV"],
  ".editorconfig": ["#8a8a8a", "EDT"],
  "cmakelists.txt": ["#3c8fb8", "CMK"],
  "requirements.txt": ["#3572a5", "PY"],
  "pyproject.toml": ["#3572a5", "PY"],
  "pipfile": ["#3572a5", "PY"],
  "tsconfig.json": ["#3178c6", "TS"],
};

// Extension → short label override (else: first three letters, uppercased).
const EXT_LABEL: Record<string, string> = {
  json: "{}",
  yaml: "YML",
  yml: "YML",
  html: "HTM",
  htm: "HTM",
  toml: "TOM",
  conf: "CFG",
  cfg: "CFG",
  ini: "INI",
  jpeg: "JPG",
  bash: "SH",
  zsh: "SH",
  fish: "SH",
};

/** Visual identity of one file name: page fill + label text. */
function iconStyle(name: string): { fill: string; dark: boolean; label: string } {
  const lower = name.toLowerCase();
  const hit = NAME_ICONS[lower];
  if (hit) return { fill: hit[0], dark: false, label: hit[1] };
  const dot = lower.lastIndexOf(".");
  const ext = dot > 0 ? lower.slice(dot + 1) : "";
  const [fill, dark] = EXT_COLORS[ext] ?? ["#6e7681", false];
  const label = ext ? (EXT_LABEL[ext] ?? ext.slice(0, 3).toUpperCase()) : "";
  return { fill, dark: dark ?? false, label };
}

/** The page/folder glyph — 16×16, sized by CSS (.file-icon). */
export function FileIcon({ name, isDir }: { name: string; isDir: boolean }) {
  if (isDir) {
    return (
      <svg className="file-icon" viewBox="0 0 16 16" width="16" height="16" aria-hidden="true">
        <rect x="1.5" y="2.5" width="5.4" height="3.4" rx="0.8" fill="#417fb0" />
        <path
          d="M1.5 4.6 h13 v7.6 a1 1 0 0 1 -1 1 h-11 a1 1 0 0 1 -1 -1 z"
          fill="#519aba"
        />
      </svg>
    );
  }
  const { fill, dark, label } = iconStyle(name);
  return (
    <svg className="file-icon" viewBox="0 0 16 16" width="16" height="16" aria-hidden="true">
      <path
        d="M3 2 a1 1 0 0 1 1 -1 h5.2 L13 4.8 v8.2 a1 1 0 0 1 -1 1 H4 a1 1 0 0 1 -1 -1 z"
        fill={fill}
      />
      <path d="M9.2 1 L13 4.8 h-2.8 a1 1 0 0 1 -1 -1 z" fill="#ffffff" opacity="0.35" />
      {label && (
        <text
          x="8"
          y="11.9"
          textAnchor="middle"
          fontSize={label.length > 2 ? 4.6 : 5.6}
          fill={dark ? "#171717" : "#ffffff"}
        >
          {label}
        </text>
      )}
    </svg>
  );
}

// Git badge: kind → letter + color class. 新增(green)/变更(blue)/删除(gray)
// are the panel's contract; conflicted gets orange as the extra state.
export type GitBadge = { letter: string; cls: "added" | "changed" | "deleted" | "conflict" };

const BADGES: Record<string, GitBadge> = {
  untracked: { letter: "U", cls: "added" },
  added: { letter: "A", cls: "added" },
  modified: { letter: "M", cls: "changed" },
  renamed: { letter: "R", cls: "changed" },
  deleted: { letter: "D", cls: "deleted" },
  conflicted: { letter: "!", cls: "conflict" },
};

/** Badge for one porcelain change (kind is server-classified; xy is the fallback). */
export function gitBadge(c: Pick<GitChange, "kind" | "xy">): GitBadge {
  if (BADGES[c.kind]) return BADGES[c.kind];
  const xy = c.xy ?? "";
  if (xy.startsWith("??")) return BADGES.untracked;
  if (xy.includes("D")) return BADGES.deleted;
  if (xy.includes("R")) return BADGES.renamed;
  return BADGES.modified;
}
