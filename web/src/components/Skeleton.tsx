// SkeletonRows: one shimmer block per would-be row (sibling-product precedent —
// loading reads as structure, not as an empty panel). Shared by the file
// list and the preview pane.
export default function SkeletonRows({ n }: { n: number }) {
  return (
    <div className="skeleton-rows" aria-hidden="true">
      {Array.from({ length: n }, (_, i) => (
        <div key={i} className="skeleton-row" style={{ width: `${55 + ((i * 17) % 40)}%` }} />
      ))}
    </div>
  );
}
