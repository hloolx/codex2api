const variants = [
  {
    program: "daybreak_red",
    label: "Daybreak Red",
    color:
      "bg-red-500/10 text-red-700 ring-red-600/20 dark:text-red-400 dark:ring-red-400/20",
  },
  {
    program: "daybreak_blue",
    label: "Daybreak Blue",
    color:
      "bg-blue-500/10 text-blue-700 ring-blue-600/20 dark:text-blue-400 dark:ring-blue-400/20",
  },
] as const;

export default function DaybreakBadge({
  models,
}: {
  models?: Record<string, string[]>;
}) {
  const programs = new Set(Object.values(models ?? {}).flat());
  const variant = variants.find(({ program }) => programs.has(program));
  if (!variant) return null;

  return (
    <span
      className={`inline-flex shrink-0 items-center rounded-md px-1.5 py-0.5 text-[11px] font-medium ring-1 ring-inset ${variant.color}`}
    >
      {variant.label}
    </span>
  );
}
