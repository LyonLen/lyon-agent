// XlsxSheet — interactive spreadsheet renderer (ported from the sibling product's
// XlsxSheet; antd Tabs/Button replaced with self-drawn controls): sheet
// tabs + column sort + cell drag-select + CSV export + sticky headers /
// row numbers. Workbook parsing (XLSX.read, including the CSV text
// variant) stays in the caller's fetch shell.
//
// Layout contract: the root is position:absolute inset:0 — the parent
// must have a definite height (lyon mounts it in .pv-office). The sheet
// itself is deliberately a light surface: it is a spreadsheet page.

import { useEffect, useMemo, useRef, useState, type CSSProperties } from "react";
import * as XLSX from "xlsx";
import { sortRows, cycleSort, type Sort } from "../xlsxSort";

const MAX_XLSX_ROWS = 5000;
const MAX_XLSX_CELLS = 100_000;

export function colLabel(i: number): string {
  let s = "";
  let n = i;
  while (true) {
    s = String.fromCharCode(65 + (n % 26)) + s;
    if (n < 26) break;
    n = Math.floor(n / 26) - 1;
  }
  return s;
}

export default function XlsxSheet({ workbook, filename, loading, err }: {
  workbook: XLSX.WorkBook | null;
  filename: string;
  loading?: boolean;
  err?: string | null;
}) {
  const [activeSheet, setActiveSheet] = useState("");
  const [range, setRange] = useState<null | { startR: number; startC: number; endR: number; endC: number }>(null);
  const draggingRef = useRef(false);
  const [sort, setSort] = useState<Sort>(null);
  const scrollRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    setActiveSheet(workbook?.SheetNames[0] ?? "");
  }, [workbook]);

  useEffect(() => {
    setRange(null);
    setSort(null);
  }, [activeSheet]);

  useEffect(() => {
    const onUp = () => {
      draggingRef.current = false;
    };
    document.addEventListener("mouseup", onUp);
    return () => document.removeEventListener("mouseup", onUp);
  }, []);

  const sheet = workbook && activeSheet ? workbook.Sheets[activeSheet] : null;

  // raw:false → dates/numbers come formatted as the cell's own format
  // string ("2026/03/27", "12%") instead of the OADate number — what
  // renders is what Excel showed.
  const rows: unknown[][] = useMemo(
    () => (sheet ? XLSX.utils.sheet_to_json<unknown[]>(sheet, { header: 1, defval: "", raw: false }) : []),
    [sheet],
  );

  const totalRows = rows.length;
  const maxCols = rows.reduce((m, r) => Math.max(m, r.length), 0);
  const totalCells = totalRows * maxCols;
  const tooManyCells = totalCells > MAX_XLSX_CELLS;
  const truncated = totalRows > MAX_XLSX_ROWS;

  // Sorting runs on the full rows (truncation slices AFTER the sort);
  // row numbers stay the original Excel line numbers.
  const sortedView = useMemo(() => sortRows(rows, sort), [rows, sort]);
  const cut = truncated ? sortedView.slice(0, MAX_XLSX_ROWS) : sortedView;
  const displayRows = cut.map((x) => x.row);
  const rowNumbers = cut.map((x) => x.rowNo);

  const rangeNorm = range && {
    r0: Math.min(range.startR, range.endR),
    r1: Math.max(range.startR, range.endR),
    c0: Math.min(range.startC, range.endC),
    c1: Math.max(range.startC, range.endC),
  };

  const handleCellMouseDown = (r: number, c: number, e: React.MouseEvent) => {
    if (e.button !== 0) return;
    draggingRef.current = true;
    setRange({ startR: r, startC: c, endR: r, endC: c });
  };

  const handleCellMouseEnter = (r: number, c: number) => {
    if (!draggingRef.current) return;
    setRange((prev) => prev ? { ...prev, endR: r, endC: c } : prev);
  };

  const rangeA1 = () => {
    if (!rangeNorm) return "";
    const start = `${colLabel(rangeNorm.c0)}${rangeNorm.r0 + 1}`;
    const end = `${colLabel(rangeNorm.c1)}${rangeNorm.r1 + 1}`;
    return rangeNorm.r0 === rangeNorm.r1 && rangeNorm.c0 === rangeNorm.c1 ? start : `${start}:${end}`;
  };

  const handleExportCsv = () => {
    if (!sheet) return;
    const csv = XLSX.utils.sheet_to_csv(sheet, { rawNumbers: false });
    // BOM so Excel opens UTF-8 Chinese correctly.
    const blob = new Blob(["﻿" + csv], { type: "text/csv;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `${filename.replace(/\.(xlsx|xls|csv)$/i, "")}·${activeSheet}.csv`;
    a.click();
    URL.revokeObjectURL(url);
  };

  if (err) {
    return (
      <div className="pv-center">
        <span className="err">{err}</span>
      </div>
    );
  }
  if (loading || !workbook) {
    return (
      <div className="pv-center">
        <span style={{ color: "var(--dim)", fontSize: 13 }}>loading…</span>
      </div>
    );
  }

  return (
    <div className="xlsx-root">
      <div className="xlsx-bar">
        <div className="xlsx-tabs">
          {workbook.SheetNames.map((n) => (
            <button
              key={n}
              className={"xlsx-tab" + (n === activeSheet ? " active" : "")}
              onClick={() => setActiveSheet(n)}
            >
              {n}
            </button>
          ))}
        </div>
        <div className="xlsx-meta">
          <span className="xlsx-stats">
            {totalRows} rows × {maxCols} cols{rangeNorm && ` · selected ${rangeA1()}`}
          </span>
          <button className="xlsx-export" onClick={handleExportCsv} disabled={!sheet}>
            export csv
          </button>
        </div>
      </div>

      {tooManyCells ? (
        <div className="xlsx-toomany">
          this sheet has too many cells ({totalCells.toLocaleString()} &gt; {MAX_XLSX_CELLS.toLocaleString()}) to preview
          in the browser — download the file and open it locally.
        </div>
      ) : (
        <>
          {truncated && (
            <div className="xlsx-truncated">
              showing the first {MAX_XLSX_ROWS} of {totalRows} rows
              {sort ? ` · sorted by column ${colLabel(sort.col)} ${sort.dir === "asc" ? "asc" : "desc"}` : ""}
              {" — "}download the file for the full data.
            </div>
          )}
          <div ref={scrollRef} className="xlsx-scroll">
            <table className="xlsx-table">
              <thead>
                <tr>
                  <th style={headCornerStyle}>#</th>
                  {Array.from({ length: maxCols }, (_, i) => (
                    <th
                      key={i}
                      onClick={() => {
                        setSort((prev) => cycleSort(prev, i));
                        setRange(null);
                      }}
                      style={{ ...headCellStyle, cursor: "pointer" }}
                    >
                      {colLabel(i)}
                      {sort?.col === i ? (sort.dir === "asc" ? " ▲" : " ▼") : ""}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {displayRows.map((row, rIdx) => (
                  <tr key={rIdx}>
                    <td style={rowNumStyle}>{rowNumbers[rIdx]}</td>
                    {Array.from({ length: maxCols }, (_, cIdx) => {
                      const v = row[cIdx];
                      const text = v === null || v === undefined ? "" : typeof v === "object" ? JSON.stringify(v) : String(v);
                      const selected = rangeNorm && rIdx >= rangeNorm.r0 && rIdx <= rangeNorm.r1 && cIdx >= rangeNorm.c0 && cIdx <= rangeNorm.c1;
                      return (
                        <td
                          key={cIdx}
                          data-r={rIdx}
                          data-c={cIdx}
                          onMouseDown={(e) => handleCellMouseDown(rIdx, cIdx, e)}
                          onMouseEnter={() => handleCellMouseEnter(rIdx, cIdx)}
                          style={{
                            ...bodyCellStyle,
                            background: selected ? "rgba(77,163,255,0.18)" : undefined,
                            cursor: "cell",
                          }}
                        >
                          {text}
                        </td>
                      );
                    })}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </div>
  );
}

const headCellStyle: CSSProperties = {
  position: "sticky",
  top: 0,
  background: "#f1f5f9",
  border: "1px solid #e2e8f0",
  padding: "4px 8px",
  textAlign: "left",
  color: "#475569",
  fontWeight: 600,
  zIndex: 1,
};

const headCornerStyle: CSSProperties = {
  ...headCellStyle,
  left: 0,
  zIndex: 2,
  minWidth: 44,
  textAlign: "center",
};

const bodyCellStyle: CSSProperties = {
  border: "1px solid #e2e8f0",
  padding: "3px 8px",
  color: "#1e293b",
  maxWidth: 360,
  overflow: "hidden",
  textOverflow: "ellipsis",
};

const rowNumStyle: CSSProperties = {
  position: "sticky",
  left: 0,
  background: "#f8fafc",
  border: "1px solid #e2e8f0",
  padding: "3px 8px",
  color: "#64748b",
  textAlign: "center",
  minWidth: 44,
  fontVariantNumeric: "tabular-nums",
};
