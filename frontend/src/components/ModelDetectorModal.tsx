import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import {
  CheckCircle2,
  ChevronDown,
  CircleAlert,
  Coins,
  Database,
  ExternalLink,
  Fingerprint,
  Loader2,
  RefreshCw,
  RotateCcw,
  Square,
  XCircle,
} from "lucide-react";
import type { AccountRow } from "../types";
import { api, getAdminKey, type ModelTraceBankStatus } from "../api";
import { useToast } from "../hooks/useToast";
import { formatAccountName } from "../lib/connectionTestModels";
import { cn } from "@/lib/utils";
import Modal from "./Modal";
import { Button } from "@/components/ui/button";
import { Select } from "@/components/ui/select";
import { SegmentedPillGroup } from "@/components/ui/segmented-pill-group";

interface DetectorResult {
  model: string;
  display_name: string;
  probability: number;
  profile_similarity: number;
  score: number;
  family: string;
  family_name: string;
  conditional_probability: number;
}

interface DetectorDiagnostic {
  index: number;
  parsed_numbers: number;
  minimum_numbers: number;
  accepted: boolean;
}

interface DetectorFamilyProbability {
  family: string;
  display_name: string;
  probability: number;
}

interface DetectorReport {
  prediction: string;
  prediction_name: string;
  probability: number;
  used_outputs: number;
  results: DetectorResult[];
  diagnostics: DetectorDiagnostic[];
  calibration: {
    queries: number;
    beta: number;
    cv_accuracy: number;
  };
  family_prediction: string;
  family_prediction_name: string;
  family_probability: number;
  family_probabilities: DetectorFamilyProbability[];
  method: string;
  candidate_scope: string;
  bank_schema: string;
  source: string;
  source_url: string;
  source_revision: string;
}

interface DetectorEvent {
  type: "start" | "progress" | "complete";
  index?: number;
  total?: number;
  attempt?: number;
  max_attempts?: number;
  concurrency?: number;
  model?: string;
  source?: string;
  source_revision?: string;
  probe_id?: string;
  status?: string;
  parsed_numbers?: number;
  minimum_numbers?: number;
  elapsed_ms?: number;
  error?: string;
  report?: DetectorReport;
}

interface AttemptLog {
  attempt: number;
  status: "ok" | "invalid" | "error";
  parsed: number;
  minimum: number;
  elapsedMs?: number;
  error?: string;
}

type DetectorStatus = "idle" | "running" | "complete" | "error" | "stopped";

const TARGET_OUTPUTS = 3;
const ATTEMPTS_PER_SAMPLE = 2;
const MAX_CONCURRENCY = 3;
const COLLAPSED_CANDIDATES = 5;
const SAMPLES_STORAGE_KEY = "model_detector_samples";

function loadSamples() {
  try {
    const stored = Number(window.localStorage.getItem(SAMPLES_STORAGE_KEY));
    if (Number.isInteger(stored) && stored >= 1 && stored <= TARGET_OUTPUTS)
      return stored;
  } catch {
    // Storage may be unavailable; fall back to the recommended count.
  }
  return TARGET_OUTPUTS;
}

function percentage(value: number | undefined, digits = 1) {
  if (!Number.isFinite(value)) return "-";
  return `${((value || 0) * 100).toFixed(digits)}%`;
}

function formatDuration(ms: number) {
  if (!Number.isFinite(ms) || ms < 0) return "-";
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
  const seconds = Math.round(ms / 1000);
  return `${Math.floor(seconds / 60)}m ${String(seconds % 60).padStart(2, "0")}s`;
}

const CONFIDENCE_TONES = {
  high: {
    labelKey: "accounts.detectorConfidenceHigh",
    stroke: "stroke-emerald-500",
    bar: "bg-emerald-500",
    badge:
      "border-emerald-200 bg-emerald-50 text-emerald-700 dark:border-emerald-900/60 dark:bg-emerald-950/40 dark:text-emerald-300",
  },
  medium: {
    labelKey: "accounts.detectorConfidenceMedium",
    stroke: "stroke-sky-500",
    bar: "bg-sky-500",
    badge:
      "border-sky-200 bg-sky-50 text-sky-700 dark:border-sky-900/60 dark:bg-sky-950/40 dark:text-sky-300",
  },
  low: {
    labelKey: "accounts.detectorConfidenceLow",
    stroke: "stroke-amber-500",
    bar: "bg-amber-500",
    badge:
      "border-amber-200 bg-amber-50 text-amber-700 dark:border-amber-900/60 dark:bg-amber-950/40 dark:text-amber-300",
  },
} as const;

function confidenceTone(probability: number) {
  if (probability >= 0.8) return CONFIDENCE_TONES.high;
  if (probability >= 0.5) return CONFIDENCE_TONES.medium;
  return CONFIDENCE_TONES.low;
}

function ProbabilityRing({
  value,
  strokeClass,
}: {
  value: number;
  strokeClass: string;
}) {
  const radius = 32;
  const circumference = 2 * Math.PI * radius;
  const clamped = Math.max(0, Math.min(1, Number.isFinite(value) ? value : 0));
  return (
    <div className="relative size-20 shrink-0">
      <svg viewBox="0 0 80 80" className="size-full -rotate-90" aria-hidden>
        <circle
          cx="40"
          cy="40"
          r={radius}
          fill="none"
          strokeWidth="7"
          className="stroke-muted"
        />
        <circle
          cx="40"
          cy="40"
          r={radius}
          fill="none"
          strokeWidth="7"
          strokeLinecap="round"
          strokeDasharray={circumference}
          strokeDashoffset={circumference * (1 - clamped)}
          className={cn(
            "transition-[stroke-dashoffset] duration-700 ease-out motion-reduce:transition-none",
            strokeClass,
          )}
        />
      </svg>
      <span className="absolute inset-0 flex items-center justify-center text-[15px] font-semibold tabular-nums">
        {percentage(clamped)}
      </span>
    </div>
  );
}

function SectionLabel({
  children,
  extra,
}: {
  children: ReactNode;
  extra?: ReactNode;
}) {
  return (
    <div className="mb-2 flex items-center justify-between gap-2 text-xs font-medium text-foreground">
      <span>{children}</span>
      {extra}
    </div>
  );
}

export default function ModelDetectorModal({
  account,
  requestModels,
  defaultModel,
  onClose,
}: {
  account: AccountRow;
  requestModels: string[];
  defaultModel: string;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const [model, setModel] = useState(defaultModel || requestModels[0] || "");
  const [samples, setSamples] = useState(loadSamples);
  const [concurrency, setConcurrency] = useState(1);
  const [status, setStatus] = useState<DetectorStatus>("idle");
  const [progress, setProgress] = useState(0);
  const [total, setTotal] = useState(samples);
  const [attempt, setAttempt] = useState(0);
  const [maxAttempts, setMaxAttempts] = useState(
    samples * ATTEMPTS_PER_SAMPLE,
  );
  const [attempts, setAttempts] = useState<AttemptLog[]>([]);
  const [startedAt, setStartedAt] = useState<number | null>(null);
  const [finishedAt, setFinishedAt] = useState<number | null>(null);
  const [now, setNow] = useState(() => Date.now());
  const [error, setError] = useState("");
  const [report, setReport] = useState<DetectorReport | null>(null);
  const [showAllCandidates, setShowAllCandidates] = useState(false);
  const abortRef = useRef<AbortController | null>(null);
  const reportRef = useRef<HTMLDivElement | null>(null);
  const { showToast } = useToast();
  const [bank, setBank] = useState<ModelTraceBankStatus | null>(null);
  const [bankBusy, setBankBusy] = useState(false);

  const running = status === "running";

  useEffect(() => () => abortRef.current?.abort(), []);

  useEffect(() => {
    if (!running) return;
    const timer = window.setInterval(() => setNow(Date.now()), 200);
    return () => window.clearInterval(timer);
  }, [running]);

  useEffect(() => {
    if (!report) return;
    const frame = window.requestAnimationFrame(() =>
      reportRef.current?.scrollIntoView({ behavior: "smooth", block: "start" }),
    );
    return () => window.cancelAnimationFrame(frame);
  }, [report]);

  useEffect(() => {
    let cancelled = false;
    api
      .getModelTraceBank()
      .then((status) => {
        if (!cancelled) setBank(status);
      })
      .catch(() => {
        // The bank bar is informational; detection still works without it.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const updateBank = async () => {
    setBankBusy(true);
    try {
      const result = await api.updateModelTraceBank();
      setBank(result);
      if (!result.updated) {
        showToast(t("accounts.detectorBankUpToDate"));
      } else if (result.added_models?.length) {
        showToast(
          t("accounts.detectorBankUpdatedWithModels", {
            models: result.added_models.join(", "),
          }),
        );
      } else {
        showToast(t("accounts.detectorBankUpdated"));
      }
    } catch (caught) {
      showToast(
        caught instanceof Error
          ? caught.message
          : t("accounts.detectorBankUpdateFailed"),
        "error",
      );
    } finally {
      setBankBusy(false);
    }
  };

  const resetBank = async () => {
    setBankBusy(true);
    try {
      setBank(await api.resetModelTraceBank());
      showToast(t("accounts.detectorBankReset"));
    } catch (caught) {
      showToast(
        caught instanceof Error
          ? caught.message
          : t("accounts.detectorBankUpdateFailed"),
        "error",
      );
    } finally {
      setBankBusy(false);
    }
  };

  const modelOptions = useMemo(() => {
    const merged = Array.from(
      new Set([defaultModel, ...requestModels].filter(Boolean)),
    );
    return merged.map((value) => ({ label: value, value }));
  }, [defaultModel, requestModels]);

  const sampleOptions = useMemo(
    () =>
      Array.from({ length: TARGET_OUTPUTS }, (_, index) => {
        const count = index + 1;
        return {
          value: String(count),
          label:
            count === TARGET_OUTPUTS ? (
              <>
                {t("accounts.detectorSamplesOption", { count })}
                <span className="rounded-full bg-primary/10 px-1.5 py-px text-[10px] font-medium text-primary">
                  {t("accounts.detectorRecommended")}
                </span>
              </>
            ) : (
              t("accounts.detectorSamplesOption", { count })
            ),
        };
      }),
    [t],
  );

  const concurrencyOptions = useMemo(
    () =>
      Array.from({ length: MAX_CONCURRENCY }, (_, index) => {
        const count = index + 1;
        return {
          value: String(count),
          label: String(count),
          disabled: count > samples,
          title:
            count > samples
              ? t("accounts.detectorConcurrencyLimited")
              : undefined,
        };
      }),
    [samples, t],
  );

  const changeSamples = (value: string) => {
    const next = Number(value);
    if (!Number.isInteger(next) || next < 1 || next > TARGET_OUTPUTS) return;
    setSamples(next);
    setConcurrency((current) => Math.min(current, next));
    try {
      window.localStorage.setItem(SAMPLES_STORAGE_KEY, String(next));
    } catch {
      // Remembering the choice is best-effort.
    }
  };

  const closeModal = () => {
    abortRef.current?.abort();
    onClose();
  };

  const stop = () => {
    abortRef.current?.abort();
    abortRef.current = null;
    setStatus("stopped");
    setFinishedAt(Date.now());
  };

  const start = async () => {
    if (!model || status === "running") return;
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    const effectiveConcurrency = Math.min(concurrency, samples);
    const startTime = Date.now();
    setStatus("running");
    setProgress(0);
    setTotal(samples);
    setAttempt(0);
    setMaxAttempts(samples * ATTEMPTS_PER_SAMPLE);
    setAttempts([]);
    setStartedAt(startTime);
    setFinishedAt(null);
    setNow(startTime);
    setError("");
    setReport(null);
    setShowAllCandidates(false);

    const finish = () => setFinishedAt(Date.now());

    try {
      const params = new URLSearchParams({
        model,
        concurrency: String(effectiveConcurrency),
        samples: String(samples),
      });
      const response = await fetch(
        `/api/admin/accounts/${account.id}/model-detector?${params.toString()}`,
        {
          signal: controller.signal,
          headers: getAdminKey() ? { "X-Admin-Key": getAdminKey() } : {},
        },
      );
      if (!response.ok) {
        const body = await response.text();
        let message = `HTTP ${response.status}`;
        try {
          const parsed = JSON.parse(body) as { error?: string };
          if (parsed.error) message = parsed.error;
        } catch {
          // Keep the status fallback.
        }
        throw new Error(message);
      }
      const reader = response.body?.getReader();
      if (!reader) throw new Error(t("accounts.detectorStreamUnsupported"));
      const decoder = new TextDecoder();
      let buffer = "";
      let sawComplete = false;
      const consume = (lines: string[]) => {
        for (const line of lines) {
          const trimmed = line.trim();
          if (!trimmed.startsWith("data: ")) continue;
          try {
            const event = JSON.parse(trimmed.slice(6)) as DetectorEvent;
            if (event.type === "start") {
              setTotal(event.total || samples);
              setMaxAttempts(event.max_attempts || samples * ATTEMPTS_PER_SAMPLE);
            } else if (event.type === "progress") {
              setProgress(event.index || 0);
              setTotal(event.total || samples);
              setAttempt(event.attempt || 0);
              setMaxAttempts(event.max_attempts || samples * ATTEMPTS_PER_SAMPLE);
              const attemptStatus =
                event.status === "ok" || event.status === "invalid"
                  ? event.status
                  : "error";
              setAttempts((current) => [
                ...current,
                {
                  attempt: event.attempt || current.length + 1,
                  status: attemptStatus,
                  parsed: event.parsed_numbers ?? 0,
                  minimum: event.minimum_numbers ?? 0,
                  elapsedMs: event.elapsed_ms,
                  error: event.error,
                },
              ]);
            } else if (event.type === "complete") {
              sawComplete = true;
              setProgress(event.index || 0);
              setTotal(event.total || samples);
              setAttempt(event.attempt || 0);
              setMaxAttempts(event.max_attempts || samples * ATTEMPTS_PER_SAMPLE);
              finish();
              if (event.report) {
                setReport(event.report);
                setStatus("complete");
              } else {
                setError(event.error || t("accounts.detectorFailed"));
                setStatus("error");
              }
            }
          } catch {
            // Ignore incomplete/non-SSE lines.
          }
        }
      };
      while (true) {
        const { done, value } = await reader.read();
        if (done) {
          buffer += decoder.decode();
          break;
        }
        buffer += decoder.decode(value, { stream: true });
        const lines = buffer.split("\n");
        buffer = lines.pop() || "";
        consume(lines);
      }
      if (buffer.trim()) consume([buffer]);
      if (!sawComplete) throw new Error(t("accounts.detectorConnectionEnded"));
    } catch (caught) {
      if (caught instanceof DOMException && caught.name === "AbortError")
        return;
      finish();
      setStatus("error");
      setError(
        caught instanceof Error ? caught.message : t("accounts.detectorFailed"),
      );
    }
  };

  const elapsedMs =
    startedAt === null ? 0 : (finishedAt ?? (running ? now : startedAt)) - startedAt;
  const rankedResults = report?.results || [];
  const visibleResults = showAllCandidates
    ? rankedResults
    : rankedResults.slice(0, COLLAPSED_CANDIDATES);
  const attemptLogs: AttemptLog[] =
    attempts.length > 0
      ? attempts
      : (report?.diagnostics || []).map((diagnostic) => ({
          attempt: diagnostic.index + 1,
          status: diagnostic.accepted ? "ok" : "invalid",
          parsed: diagnostic.parsed_numbers,
          minimum: diagnostic.minimum_numbers,
        }));
  const hasRun = status !== "idle";
  const tone = report ? confidenceTone(report.probability) : null;

  const attemptList =
    attemptLogs.length > 0 || running ? (
      <div className="flex flex-wrap gap-1.5">
        {attemptLogs.map((log) => {
          const Icon =
            log.status === "ok"
              ? CheckCircle2
              : log.status === "invalid"
                ? CircleAlert
                : XCircle;
          return (
            <span
              key={`${log.attempt}-${log.status}`}
              title={
                log.error ||
                t("accounts.detectorParsedNumbers", {
                  parsed: log.parsed,
                  minimum: log.minimum,
                })
              }
              className={cn(
                "inline-flex items-center gap-1 rounded-md border px-2 py-1 text-[11px] tabular-nums",
                log.status === "ok" &&
                  "border-emerald-200 bg-emerald-50/60 text-emerald-700 dark:border-emerald-900/60 dark:bg-emerald-950/30 dark:text-emerald-400",
                log.status === "invalid" &&
                  "border-amber-200 bg-amber-50/60 text-amber-700 dark:border-amber-900/60 dark:bg-amber-950/30 dark:text-amber-300",
                log.status === "error" &&
                  "border-red-200 bg-red-50/60 text-red-700 dark:border-red-900/60 dark:bg-red-950/30 dark:text-red-400",
              )}
            >
              <Icon className="size-3" />
              {log.status === "error"
                ? t("accounts.detectorAttemptFailed", { index: log.attempt })
                : t("accounts.detectorDiagnosticItem", {
                    index: log.attempt,
                    parsed: log.parsed,
                    minimum: log.minimum,
                  })}
              {log.elapsedMs !== undefined ? (
                <span className="opacity-70">
                  · {formatDuration(log.elapsedMs)}
                </span>
              ) : null}
            </span>
          );
        })}
        {running ? (
          <span className="inline-flex items-center gap-1 rounded-md border border-dashed border-border px-2 py-1 text-[11px] text-muted-foreground">
            <Loader2 className="size-3 animate-spin" />
            {t("accounts.detectorWaiting")}
          </span>
        ) : null}
      </div>
    ) : null;

  return (
    <Modal
      show={true}
      title={
        <span className="flex min-w-0 items-center gap-3">
          <span className="flex size-9 shrink-0 items-center justify-center rounded-xl bg-primary/10 text-primary ring-1 ring-primary/15">
            <Fingerprint className="size-[18px]" />
          </span>
          <span className="min-w-0">
            <span className="block">{t("accounts.detectorTitle")}</span>
            <span className="block truncate text-xs font-normal text-muted-foreground">
              {formatAccountName(account)}
            </span>
          </span>
        </span>
      }
      onClose={closeModal}
      footer={
        <div className="flex w-full flex-wrap items-center justify-end gap-2">
          {hasRun && !running && startedAt !== null ? (
            <span className="mr-auto text-[11px] tabular-nums text-muted-foreground">
              {t("accounts.detectorElapsed", { time: formatDuration(elapsedMs) })}
            </span>
          ) : null}
          {running ? (
            <>
              <Button variant="outline" onClick={stop}>
                <Square className="size-3 fill-current" />
                {t("accounts.detectorStop")}
              </Button>
              <Button disabled>
                <Loader2 className="size-3.5 animate-spin" />
                {t("accounts.detectorRunning")}
              </Button>
            </>
          ) : (
            <>
              <Button variant="outline" onClick={closeModal}>
                {t("common.close")}
              </Button>
              <Button disabled={!model} onClick={() => void start()}>
                {hasRun ? (
                  <RotateCcw className="size-3.5" />
                ) : (
                  <Fingerprint className="size-3.5" />
                )}
                {hasRun
                  ? t("accounts.detectorRestart")
                  : t("accounts.detectorStart")}
              </Button>
            </>
          )}
        </div>
      }
      contentClassName="sm:max-w-[720px]"
    >
      <div className="space-y-4">
        <div className="overflow-hidden rounded-xl border border-border bg-muted/20">
          <div className="space-y-4 p-4">
            <label className="block min-w-0 space-y-1.5">
              <span className="text-xs font-medium">
                {t("accounts.detectorRequestModel")}
              </span>
              <Select
                value={model}
                onValueChange={setModel}
                options={modelOptions}
                disabled={running}
                aria-label={t("accounts.detectorRequestModel")}
              />
            </label>

            <div className="grid gap-4 sm:grid-cols-[minmax(0,1.5fr)_minmax(0,1fr)]">
              <div className="min-w-0 space-y-1.5">
                <div className="text-xs font-medium">
                  {t("accounts.detectorSamples")}
                </div>
                <SegmentedPillGroup
                  value={String(samples)}
                  onChange={changeSamples}
                  options={sampleOptions}
                  label={t("accounts.detectorSamples")}
                  disabled={running}
                />
                <p className="text-[11px] text-muted-foreground">
                  {t("accounts.detectorSamplesHint")}
                </p>
              </div>
              <div className="min-w-0 space-y-1.5">
                <div className="text-xs font-medium">
                  {t("accounts.detectorConcurrency")}
                </div>
                <SegmentedPillGroup
                  value={String(Math.min(concurrency, samples))}
                  onChange={(value) => setConcurrency(Number(value))}
                  options={concurrencyOptions}
                  label={t("accounts.detectorConcurrency")}
                  disabled={running}
                />
                <p className="text-[11px] text-muted-foreground">
                  {t("accounts.detectorConcurrencyHint")}
                </p>
              </div>
            </div>

            <div className="flex items-start gap-2 rounded-lg bg-amber-500/10 px-3 py-2 text-[11px] leading-relaxed text-amber-800 dark:text-amber-300">
              <Coins className="mt-px size-3.5 shrink-0" />
              <span>
                {t("accounts.detectorDisclaimer", {
                  samples,
                  attempts: samples * ATTEMPTS_PER_SAMPLE,
                })}
              </span>
            </div>
          </div>

          {bank ? (
            <div className="flex flex-wrap items-center justify-between gap-2 border-t border-border bg-background/60 px-4 py-2 text-[11px] text-muted-foreground">
              <span
                className="flex min-w-0 items-center gap-1.5"
                title={bank.active.models.join("\n")}
              >
                <Database className="size-3.5 shrink-0" />
                <span className="min-w-0">
                  {t("accounts.detectorBankSummary", {
                    count: bank.active.models.length,
                    revision: bank.active.revision.slice(0, 7),
                    date: bank.active.built_at.slice(0, 10),
                  })}
                  {" · "}
                  {bank.active.origin === "override"
                    ? t("accounts.detectorBankOriginOverride")
                    : t("accounts.detectorBankOriginEmbedded")}
                  {bank.override_stale ? (
                    <span className="ml-1 text-amber-700 dark:text-amber-300">
                      {t("accounts.detectorBankOverrideStale")}
                    </span>
                  ) : null}
                  {bank.override_error ? (
                    <span className="ml-1 text-amber-700 dark:text-amber-300">
                      {t("accounts.detectorBankOverrideInvalid")}
                    </span>
                  ) : null}
                </span>
              </span>
              <span className="flex items-center gap-1">
                {bank.override ? (
                  <Button
                    size="xs"
                    variant="ghost"
                    className="text-[11px]"
                    disabled={bankBusy || running}
                    onClick={() => void resetBank()}
                  >
                    <RotateCcw />
                    {t("accounts.detectorBankResetAction")}
                  </Button>
                ) : null}
                <Button
                  size="xs"
                  variant="ghost"
                  className="text-[11px]"
                  disabled={bankBusy || running}
                  onClick={() => void updateBank()}
                >
                  <RefreshCw className={cn(bankBusy && "animate-spin")} />
                  {t("accounts.detectorBankUpdateAction")}
                </Button>
              </span>
            </div>
          ) : null}
        </div>

        {status === "idle" ? (
          <div className="flex flex-col items-center gap-2 rounded-xl border border-dashed border-border px-6 py-7 text-center">
            <span className="flex size-10 items-center justify-center rounded-full bg-muted text-muted-foreground">
              <Fingerprint className="size-5" />
            </span>
            <p className="max-w-md text-xs leading-relaxed text-muted-foreground">
              {t("accounts.detectorIdleHint")}
            </p>
          </div>
        ) : null}

        {running ? (
          <div
            className="space-y-3 rounded-xl border border-primary/25 bg-primary/[0.04] px-4 py-3.5"
            aria-live="polite"
          >
            <div className="flex items-center justify-between gap-3 text-xs">
              <span className="flex items-center gap-1.5 font-medium">
                <Loader2 className="size-3.5 animate-spin text-primary" />
                {t("accounts.detectorRunning")}
              </span>
              <span className="tabular-nums text-muted-foreground">
                {t("accounts.detectorElapsed", {
                  time: formatDuration(elapsedMs),
                })}
              </span>
            </div>
            <div className="flex gap-1.5" aria-hidden>
              {Array.from({ length: Math.max(total, 1) }, (_, index) => (
                <div
                  key={index}
                  className={cn(
                    "h-1.5 flex-1 rounded-full transition-colors duration-300",
                    index < progress
                      ? "bg-primary"
                      : index === progress
                        ? "animate-pulse bg-primary/30"
                        : "bg-muted",
                  )}
                />
              ))}
            </div>
            <div className="flex flex-wrap items-center justify-between gap-2 text-[11px] text-muted-foreground">
              <span>
                {t("accounts.detectorProgress")}{" "}
                <span className="font-medium tabular-nums text-foreground">
                  {progress} / {total}
                </span>
              </span>
              <span className="tabular-nums">
                {t("accounts.detectorAttempt", {
                  current: attempt,
                  total: maxAttempts,
                })}
              </span>
            </div>
            {attemptList}
          </div>
        ) : null}

        {status === "stopped" || status === "error" ? (
          <div
            className={cn(
              "space-y-3 rounded-xl border px-4 py-3",
              status === "error"
                ? "border-red-200 bg-red-50 dark:border-red-900/50 dark:bg-red-950/30"
                : "border-border bg-muted/30",
            )}
            aria-live="polite"
          >
            <div
              className={cn(
                "flex items-start gap-2 text-xs",
                status === "error"
                  ? "text-red-700 dark:text-red-400"
                  : "text-muted-foreground",
              )}
            >
              {status === "error" ? (
                <XCircle className="mt-px size-4 shrink-0" />
              ) : (
                <Square className="mt-0.5 size-3 shrink-0 fill-current" />
              )}
              <span className="min-w-0 break-words">
                {status === "error"
                  ? error
                  : t("accounts.detectorStopped")}
              </span>
            </div>
            {attemptList}
          </div>
        ) : null}

        {report && tone ? (
          <div
            ref={reportRef}
            className="scroll-mt-2 overflow-hidden rounded-xl border border-border"
            aria-live="polite"
          >
            <div className="flex flex-col gap-4 bg-gradient-to-br from-primary/[0.06] via-transparent to-transparent px-4 py-4 sm:flex-row sm:items-center">
              <ProbabilityRing
                value={report.probability}
                strokeClass={tone.stroke}
              />
              <div className="min-w-0 flex-1 space-y-1.5">
                <div className="flex flex-wrap items-center gap-2 text-[11px] text-muted-foreground">
                  <span>{t("accounts.detectorPrediction")}</span>
                  <span
                    className={cn(
                      "rounded-full border px-1.5 py-px text-[10px] font-medium",
                      tone.badge,
                    )}
                  >
                    {t(tone.labelKey)}
                  </span>
                </div>
                <div className="break-all font-mono text-lg font-semibold leading-tight">
                  {report.prediction_name || report.prediction}
                </div>
                <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-muted-foreground">
                  <span>
                    {t("accounts.detectorProbability")}{" "}
                    <span className="font-medium tabular-nums text-foreground">
                      {percentage(report.probability)}
                    </span>
                  </span>
                  {report.family_prediction_name ? (
                    <span>
                      {t("accounts.detectorFamily")}{" "}
                      <span className="font-medium text-foreground">
                        {report.family_prediction_name}
                      </span>{" "}
                      <span className="tabular-nums">
                        ({percentage(report.family_probability)})
                      </span>
                    </span>
                  ) : null}
                  <span className="min-w-0 break-all">
                    {t("accounts.detectorRequestedModel", { model })}
                  </span>
                </div>
              </div>
            </div>

            {rankedResults.length > 0 ? (
              <div className="border-t border-border px-4 py-3">
                <SectionLabel
                  extra={
                    <span className="hidden gap-3 pr-2 text-[10px] font-normal text-muted-foreground sm:flex">
                      <span className="w-14 text-right">
                        {t("accounts.detectorProbabilityShort")}
                      </span>
                      <span className="w-14 text-right">
                        {t("accounts.detectorSimilarityShort")}
                      </span>
                    </span>
                  }
                >
                  {t("accounts.detectorCandidateRanking")}
                </SectionLabel>
                <div className="space-y-0.5">
                  {visibleResults.map((result, index) => {
                    const top = index === 0;
                    return (
                      <div
                        key={result.model}
                        className={cn(
                          "grid grid-cols-[1.25rem_minmax(0,9rem)_minmax(3rem,1fr)_3.5rem] items-center gap-2 rounded-lg px-2 py-1.5 text-xs sm:grid-cols-[1.25rem_minmax(0,12rem)_minmax(6rem,1fr)_3.5rem_3.5rem] sm:gap-3",
                          top && "bg-primary/[0.06]",
                        )}
                      >
                        <span
                          className={cn(
                            "text-center text-[11px] tabular-nums",
                            top
                              ? "font-semibold text-primary"
                              : "text-muted-foreground",
                          )}
                        >
                          {index + 1}
                        </span>
                        <span className="min-w-0">
                          <span
                            className={cn(
                              "block truncate font-mono",
                              top && "font-semibold",
                            )}
                            title={result.display_name || result.model}
                          >
                            {result.display_name || result.model}
                          </span>
                          {result.family_name ? (
                            <span className="block truncate text-[10px] text-muted-foreground">
                              {result.family_name}
                            </span>
                          ) : null}
                        </span>
                        <div className="h-1.5 overflow-hidden rounded-full bg-muted">
                          <div
                            className={cn(
                              "h-full rounded-full transition-[width] duration-500",
                              top ? tone.bar : "bg-primary/35",
                            )}
                            style={{
                              width: `${Math.max(0, Math.min(100, result.probability * 100))}%`,
                            }}
                          />
                        </div>
                        <span
                          className={cn(
                            "text-right tabular-nums",
                            top ? "font-medium" : "text-muted-foreground",
                          )}
                          title={t("accounts.detectorProbability")}
                        >
                          {percentage(result.probability)}
                        </span>
                        <span
                          className="hidden text-right tabular-nums text-muted-foreground sm:inline"
                          title={t("accounts.detectorProfileSimilarity")}
                        >
                          {percentage(result.profile_similarity)}
                        </span>
                      </div>
                    );
                  })}
                </div>
                {rankedResults.length > COLLAPSED_CANDIDATES ? (
                  <Button
                    variant="ghost"
                    size="xs"
                    className="mt-1.5 text-[11px] text-muted-foreground"
                    onClick={() => setShowAllCandidates((value) => !value)}
                  >
                    <ChevronDown
                      className={cn(
                        "transition-transform",
                        showAllCandidates && "rotate-180",
                      )}
                    />
                    {showAllCandidates
                      ? t("accounts.detectorShowLess")
                      : t("accounts.detectorShowAll", {
                          count: rankedResults.length,
                        })}
                  </Button>
                ) : null}
              </div>
            ) : null}

            {attemptList ? (
              <div className="border-t border-border px-4 py-3">
                <SectionLabel
                  extra={
                    <span className="text-[11px] font-normal text-muted-foreground">
                      {report.used_outputs} {t("accounts.detectorValidSamples")}
                    </span>
                  }
                >
                  {t("accounts.detectorDiagnostics")}
                </SectionLabel>
                {attemptList}
              </div>
            ) : null}

            <div className="space-y-1 border-t border-border bg-muted/20 px-4 py-3 text-[11px] leading-relaxed text-muted-foreground">
              <div>{t("accounts.detectorClosedSetWarning")}</div>
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                <span>
                  {t("accounts.detectorCalibration", {
                    accuracy: percentage(report.calibration.cv_accuracy),
                  })}
                </span>
                {report.source_url ? (
                  <a
                    className="inline-flex items-center gap-1 text-primary hover:underline"
                    href={report.source_url}
                    target="_blank"
                    rel="noreferrer"
                  >
                    {report.source}
                    <ExternalLink className="size-3" />
                  </a>
                ) : null}
              </div>
            </div>
          </div>
        ) : null}
      </div>
    </Modal>
  );
}
