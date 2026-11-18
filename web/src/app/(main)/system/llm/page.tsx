"use client";

import * as React from "react";

import {
  AlertCircleIcon,
  Loader2Icon,
  PlugZapIcon,
  PlusIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  SaveIcon,
  StarIcon,
  Trash2Icon,
  ZapIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import type {
  LLMAuthMode,
  LLMCredential,
  LLMCredentialProvider,
  LLMModelCatalogEntry,
  LLMPoolMember,
  LLMPoolStatus,
  LLMProfile,
  LLMRetryOverride,
} from "@/lib/types";
import { cn } from "@/lib/utils";

import { AccountsPanel } from "./_components/accounts";
import { ProfileRetryFields, RetryPolicyPanel, ZERO_OVERRIDE } from "./_components/retry";
import { setVisibleInterval } from "@/lib/polling";

// The thinking switch (thinking.type) and the thinking effort (reasoning_effort) are two [independent] fields,
// each set on its own — some APIs have no thinking field and activate thinking via the effort parameter alone, hence the decoupling.
// An empty string in storage = the field is [not sent]; Radix Select rejects an empty value, so the UI uses "none"
// as the sentinel for not sent, converted to and from "" on load and save (NONE / fromStore / toStore).
const NONE = "none";
const fromStore = (v?: string) => (v ? v : NONE);
const toStore = (v: string) => (v === NONE ? "" : v);
const THINKING_TYPES: { value: string; label: string }[] = [
  { value: NONE, label: "Not sent (default)" },
  { value: "disabled", label: "Disabled" },
  { value: "enabled", label: "Enabled" },
];
// Which request field name carries the output cap (only meaningful for the openai format). NONE ↔ "" shares the same sentinel conversion.
const MAX_TOKENS_FIELDS: { value: string; label: string }[] = [
  { value: NONE, label: "max_tokens (default)" },
  { value: "max_completion_tokens", label: "max_completion_tokens" },
];
// The other formats hard-code their field names, so the option is meaningless for them; the hint texts spell this out.
const MAX_TOKENS_FIELD_HINTS: Record<string, string> = {
  openai:
    "Which key carries the cap. max_tokens is the default and the vast majority of compatible gateways only accept it; OpenAI's official reasoning models (the o series / GPT-5) conversely only accept max_completion_tokens and fail outright with unsupported_parameter when they receive max_tokens.",
  anthropic: "Only selectable for the openai format. Anthropic's field name is fixed as max_tokens.",
  "openai-responses":
    "Only selectable for the openai format. The Responses API's field name is fixed as max_output_tokens.",
  ollama: "Only selectable for the openai format. Ollama's field name is fixed as max_tokens.",
};
// LLM profile formats. The default in the create form is "anthropic"; "ollama" is the
// keyless local-server format (base URL defaults to http://127.0.0.1:11434/v1).
type ProfileFormat = "anthropic" | "openai" | "openai-responses" | "ollama";
const OLLAMA_DEFAULT_BASE_URL = "http://127.0.0.1:11434/v1";
const EFFORT_LEVELS: { value: string; label: string }[] = [
  { value: NONE, label: "Not sent (default)" },
  { value: "low", label: "low" },
  { value: "medium", label: "medium" },
  { value: "high", label: "high" },
  { value: "xhigh", label: "xhigh" },
  { value: "max", label: "max" },
];
const MANAGED_NATIVE: Record<LLMCredentialProvider, { format: ProfileFormat; endpoint: string; userAgent: string }> = {
  "anthropic-oauth": {
    format: "anthropic",
    endpoint: "https://api.anthropic.com/v1/messages?beta=true",
    userAgent: "claude-cli/2.1.321 (external, cli)",
  },
  "openai-codex-oauth": {
    format: "openai-responses",
    endpoint: "https://chatgpt.com/backend-api/codex/responses",
    userAgent: "artex/1",
  },
};

function cooldownText(secs: number) {
  if (secs <= 0) return "";
  if (secs < 60) return `${secs}s`;
  return `${Math.ceil(secs / 60)}min`;
}

// Profile cards report credential readiness separately from the pool breaker.
// Native "session stored" never implies model entitlement, quota, balance, or billing.
type Health = { label: string; cls: string; hint?: string };
function healthOf(p: LLMProfile, m?: LLMPoolMember, credential?: LLMCredential): Health {
  if ((p.auth_mode ?? "api_key") === "oauth") {
    if (!p.credential_id) {
      return {
        label: "No account selected",
        cls: "border-destructive/50 text-destructive",
        hint: "Select a connected account before using this profile",
      };
    }
    if (!credential) {
      return {
        label: "Account unavailable",
        cls: "border-destructive/50 text-destructive",
        hint: "The selected account could not be found",
      };
    }
    if (credential.status === "reauth_required") {
      return {
        label: "Reauth required",
        cls: "border-amber-500/50 text-amber-600 dark:text-amber-400",
      };
    }
    if (credential.status === "revoked") {
      return {
        label: "Disconnected",
        cls: "border-muted-foreground/40 text-muted-foreground",
      };
    }
  } else if (!p.api_key_hint && p.format !== "ollama") {
    return {
      label: "No Key configured",
      cls: "border-muted-foreground/40 text-muted-foreground",
      hint: "No API Key set; cannot be called",
    };
  }
  if (m?.state === "tripped") {
    return {
      label: m.cooldown_secs > 0 ? `Tripped · ${cooldownText(m.cooldown_secs)}` : "Tripped",
      cls: "border-destructive/50 text-destructive",
      hint: m.last_error,
    };
  }
  if (m?.state === "degraded") {
    return {
      label: `Degraded · failed ${m.fails} times`,
      cls: "border-amber-500/50 text-amber-600 dark:text-amber-400",
      hint: m.last_error,
    };
  }
  if ((p.auth_mode ?? "api_key") === "oauth") {
    return {
      label: "Session stored",
      cls: "border-emerald-500/50 text-emerald-600 dark:text-emerald-400",
      hint: "Stored session only; model access and quota are checked when used",
    };
  }
  return { label: "Healthy", cls: "border-emerald-500/50 text-emerald-600 dark:text-emerald-400" };
}

// ─────────────────────────────────────────────────────────────────────────────
// Pool settings drawer
// ─────────────────────────────────────────────────────────────────────────────

function PoolSheet({
  open,
  onOpenChange,
  pool,
  onReload,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  pool: LLMPoolStatus | null;
  onReload: () => Promise<void>;
}) {
  const [busy, setBusy] = React.useState(false);

  // The cooldown countdown is the remaining seconds computed by the backend — fetched on a timer only while the drawer is open and some profile is unhealthy, to keep it ticking.
  React.useEffect(() => {
    if (!open || !pool?.enabled || !pool.chain.some((m) => m.state !== "ok")) return;
    const stopPolling = setVisibleInterval(() => void onReload(), 10_000);
    return stopPolling;
  }, [open, pool, onReload]);

  async function toggle(patch: { llm_pool_enabled?: boolean; llm_pool_bind_fallback?: boolean }) {
    if (busy) return;
    setBusy(true);
    try {
      await api.setSettings(patch);
      await onReload();
      if (patch.llm_pool_enabled !== undefined) {
        toast.success(patch.llm_pool_enabled ? "LLM pool enabled" : "LLM pool disabled");
      } else {
        toast.success("Fallback settings updated");
      }
    } catch (e) {
      toast.error(`Failed to apply settings: ${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  }

  async function recover(id?: string) {
    try {
      await api.resetLLMPool(id);
      await onReload();
      toast.success(id ? "Profile recovered" : "All profiles recovered");
    } catch (e) {
      toast.error(`Recovery failed: ${(e as Error).message}`);
    }
  }

  const enabled = pool?.enabled ?? false;
  const chain = pool?.chain ?? [];
  // Members taking part in the pool (excluding those flagged "excluded from pool"), in the exact order the backend tries them.
  const inChain = chain.filter((m) => m.active || !m.excluded);
  const tripped = chain.filter((m) => m.state === "tripped");

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="flex w-full flex-col gap-0 p-0 data-[side=right]:sm:max-w-lg">
        <SheetHeader className="px-4">
          <SheetTitle className="flex items-center gap-2">
            <ZapIcon className="size-4" /> LLM Pool · Failover
          </SheetTitle>
          <SheetDescription>
            When enabled, an Agent <b>without a pinned model</b> automatically fails over to the next config when the
            current one is unusable (insufficient balance / dead Key / rate limiting / service error).
          </SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-4 pb-6">
          <div className="flex items-center justify-between gap-4 rounded-lg border p-3">
            <div className="grid gap-0.5">
              <Label className="text-sm">Enable pool</Label>
              <p className="text-muted-foreground text-xs">
                Off by default. When off, only the active profile is ever used and a failure stays a failure.
              </p>
            </div>
            <Switch
              checked={enabled}
              disabled={busy}
              onCheckedChange={(v) => void toggle({ llm_pool_enabled: v })}
              aria-label="LLM pool toggle"
            />
          </div>

          {enabled && (
            <>
              <div className="flex items-center justify-between gap-4 rounded-lg border p-3">
                <div className="grid gap-0.5">
                  <Label className="text-sm">Also fall back when a pinned config fails</Label>
                  <p className="text-muted-foreground text-xs">
                    Off by default: when an Agent or task pins a config, only that config is used and a failure stays a
                    failure (no silent switch to another model). When enabled, a failing pinned config also falls back
                    to the pool chain below.
                  </p>
                </div>
                <Switch
                  checked={pool?.bind_fallback ?? false}
                  disabled={busy}
                  onCheckedChange={(v) => void toggle({ llm_pool_bind_fallback: v })}
                  aria-label="Bound-config failure fallback toggle"
                />
              </div>

              <Separator />

              <div className="grid gap-2">
                <div className="flex items-center justify-between">
                  <Label className="text-sm">Pool order</Label>
                  {tripped.length > 0 && (
                    <Button size="sm" variant="ghost" onClick={() => void recover()}>
                      <RotateCcwIcon /> Recover all
                    </Button>
                  )}
                </div>
                {inChain.length < 2 && (
                  <p className="text-muted-foreground text-xs">
                    Only {inChain.length} participating profile(s) are in the chain, so the pool has no effect — at
                    least 2 configured profiles with pool participation are required.
                  </p>
                )}
                {chain.map((m) => {
                  const excluded = m.excluded && !m.active;
                  const order = excluded ? null : inChain.findIndex((x) => x.profile_id === m.profile_id) + 1;
                  return (
                    <div
                      key={m.profile_id}
                      className={cn(
                        "grid gap-1 rounded-lg border p-2.5 text-sm",
                        excluded && "opacity-55",
                        m.state === "tripped" && "border-destructive/40",
                      )}
                    >
                      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                        <span className="w-5 shrink-0 text-center font-mono text-muted-foreground text-xs">
                          {order ?? "—"}
                        </span>
                        <span className="font-medium">{m.name}</span>
                        {m.active && (
                          <Badge variant="outline" className="border-amber-400/50 text-amber-500">
                            Active
                          </Badge>
                        )}
                        {excluded && <Badge variant="outline">Excluded from pool</Badge>}
                        <div className="ml-auto flex items-center gap-2">
                          {m.state === "tripped" && m.cooldown_secs > 0 && (
                            <span className="text-muted-foreground text-xs">
                              Cooldown {cooldownText(m.cooldown_secs)}
                            </span>
                          )}
                          {m.state === "degraded" && (
                            <span className="text-muted-foreground text-xs">{m.fails} consecutive failures</span>
                          )}
                          {m.state !== "ok" && (
                            <Button
                              size="icon"
                              variant="ghost"
                              className="size-7"
                              aria-label="Recover now"
                              title="Recover now: clears the breaker so the next call retries this profile"
                              onClick={() => void recover(m.profile_id)}
                            >
                              <RotateCcwIcon className="size-3.5" />
                            </Button>
                          )}
                        </div>
                      </div>
                      <div className="flex flex-wrap items-center gap-x-3 pl-7 text-muted-foreground text-xs">
                        <code className="truncate font-mono">{m.model}</code>
                        {!m.active && <span>Priority {m.priority}</span>}
                      </div>
                      {m.last_error && (
                        <p className="truncate pl-7 font-mono text-muted-foreground text-xs" title={m.last_error}>
                          {m.last_error}
                        </p>
                      )}
                    </div>
                  );
                })}
                {chain.length === 0 && (
                  <div className="rounded-lg border border-dashed p-4 text-center text-muted-foreground text-sm">
                    No profiles yet
                  </div>
                )}
              </div>

              <div className="rounded-lg border border-dashed p-3 text-muted-foreground text-xs leading-relaxed">
                The active profile is always first; the rest follow priority from high to low (set per profile). After a
                failure a profile enters a cooldown (60s → 5min → 30min), is skipped while cooling down, and switches
                back automatically once recovered. Profiles whose context window cannot fit the current request are
                skipped. Agents and tasks with a pinned model do not join the pool by default.
              </div>
            </>
          )}
        </div>
      </SheetContent>
    </Sheet>
  );
}

// ─────────────────────────────────────────────────────────────────────────────
// Model profile drawer (create / edit share the same form)
// ─────────────────────────────────────────────────────────────────────────────

function ProfileSheet({
  profile,
  credentials,
  credentialsError,
  open,
  onOpenChange,
  onSaved,
}: {
  profile: LLMProfile | null; // null = create
  credentials: LLMCredential[];
  credentialsError: string;
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onSaved: (id: string) => void;
}) {
  const isNew = !profile;
  const [authMode, setAuthMode] = React.useState<LLMAuthMode>("api_key");
  const [credentialID, setCredentialID] = React.useState("");
  const [name, setName] = React.useState("");
  const [format, setFormat] = React.useState<ProfileFormat>("anthropic");
  const [model, setModel] = React.useState("");
  const [baseUrl, setBaseUrl] = React.useState("");
  const [proxy, setProxy] = React.useState("");
  const [apiKey, setApiKey] = React.useState("");
  const [keyHint, setKeyHint] = React.useState("");
  const [rps, setRps] = React.useState("0");
  const [rpm, setRpm] = React.useState("0");
  const [cw, setCw] = React.useState("0"); // context window (K tokens); 0 = default 200K
  const [thinkingType, setThinkingType] = React.useState(NONE);
  const [effort, setEffort] = React.useState(NONE);
  const [priority, setPriority] = React.useState("0"); // pool order; higher = tried earlier
  const [poolExclude, setPoolExclude] = React.useState(false);
  const [streaming, setStreaming] = React.useState(true); // true = streaming (default); false = non-streaming
  const [maxTokens, setMaxTokens] = React.useState("0"); // per-reply output cap; 0 = not sent
  const [maxTokensField, setMaxTokensField] = React.useState(NONE); // which field name carries the cap; NONE = max_tokens
  const [sessionHeaderKey, setSessionHeaderKey] = React.useState(""); // custom session header name; empty = not sent
  const [userAgent, setUserAgent] = React.useState(""); // custom User-Agent; empty = keep the default UA
  const [retry, setRetry] = React.useState<LLMRetryOverride>(ZERO_OVERRIDE); // retry override for this profile; all 0 = follow the global
  const [testing, setTesting] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  const [models, setModels] = React.useState<Array<string | LLMModelCatalogEntry>>([]);
  const [loadingModels, setLoadingModels] = React.useState(false);
  const [modelsOpen, setModelsOpen] = React.useState(false);
  const [modelQuery, setModelQuery] = React.useState(""); // filter text for the loaded model list; reset on open/close

  // Every time it opens, the form is re-filled from the passed-in profile (reset to defaults when creating). Closing and reopening
  // the drawer is a clean start, leaving no leftovers from the previous profile.
  React.useEffect(() => {
    if (!open) return;
    setAuthMode(profile?.auth_mode ?? "api_key");
    setCredentialID(profile?.credential_id ? String(profile.credential_id) : "");
    setName(profile?.name ?? "");
    setFormat(
      profile?.format === "openai" || profile?.format === "openai-responses" || profile?.format === "ollama"
        ? profile.format
        : "anthropic",
    );
    setModel(profile?.model ?? "");
    setBaseUrl(profile?.base_url ?? "");
    setProxy(profile?.proxy ?? "");
    setRps(String(profile?.rate_per_second ?? 0));
    setRpm(String(profile?.rate_per_minute ?? 0));
    setCw(String(profile?.context_window_k ?? 0));
    setThinkingType(fromStore(profile?.thinking_type));
    setEffort(fromStore(profile?.reasoning_effort));
    setPriority(String(profile?.priority ?? 0));
    setPoolExclude(profile?.pool_exclude ?? false);
    setStreaming(profile?.streaming ?? true);
    setMaxTokens(String(profile?.max_tokens ?? 0));
    setMaxTokensField(fromStore(profile?.max_tokens_field));
    setSessionHeaderKey(profile?.session_header_key ?? "");
    setUserAgent(profile?.user_agent ?? "");
    setRetry(profile?.retry ?? ZERO_OVERRIDE);
    setApiKey("");
    setKeyHint(profile?.api_key_hint ?? "");
    setModels([]);
    setModelsOpen(false);
    setModelQuery("");
  }, [open, profile]);

  const profileId = profile ? Number(profile.id) : undefined;
  const credentialIDNumber = Number(credentialID);
  const hasCredentialSelection = Number.isInteger(credentialIDNumber) && credentialIDNumber > 0;
  const selectedCredential = credentials.find((credential) => credential.id === credentialIDNumber);
  const storedNativeSelection =
    profile?.auth_mode === "oauth" && profile.credential_id === credentialIDNumber && profileId !== undefined;
  const selectedNativeProvider: LLMCredentialProvider | undefined =
    selectedCredential?.provider ??
    (storedNativeSelection
      ? format === "anthropic"
        ? "anthropic-oauth"
        : format === "openai-responses"
          ? "openai-codex-oauth"
          : undefined
      : undefined);
  const managedNative = selectedNativeProvider ? MANAGED_NATIVE[selectedNativeProvider] : undefined;
  const selectedCatalog = models.find(
    (entry): entry is LLMModelCatalogEntry => typeof entry !== "string" && entry.id === model,
  );
  const existingNativeModel = profile?.auth_mode === "oauth" && profile.model === model;
  const outputCapSupported =
    authMode !== "oauth" ||
    selectedCatalog?.supports_output_cap === true ||
    (existingNativeModel && (profile.max_tokens ?? 0) > 0);
  const supportsThinkingDisable =
    authMode !== "oauth" ||
    selectedCatalog?.supports_thinking_disable === true ||
    (existingNativeModel && profile.thinking_type === "disabled");
  const thinkingOptions = supportsThinkingDisable
    ? THINKING_TYPES
    : THINKING_TYPES.filter((option) => option.value !== "disabled");
  const effortOptions = selectedCatalog?.reasoning_efforts?.length
    ? [
        { value: NONE, label: "Not sent (default)" },
        ...selectedCatalog.reasoning_efforts.map((value) => ({ value, label: value })),
      ]
    : authMode === "oauth"
      ? profile?.reasoning_effort && existingNativeModel
        ? [
            { value: NONE, label: "Not sent (default)" },
            { value: profile.reasoning_effort, label: profile.reasoning_effort },
          ]
        : [{ value: NONE, label: "Not sent (load catalog for options)" }]
      : EFFORT_LEVELS;

  function changeAuthMode(next: LLMAuthMode) {
    setAuthMode(next);
    setCredentialID("");
    setModels([]);
    setModelsOpen(false);
    if (next === "oauth") {
      setBaseUrl("");
      setApiKey("");
      setKeyHint("");
      setUserAgent("");
      setCw("0");
      setSessionHeaderKey("");
      setMaxTokens("0");
      setMaxTokensField(NONE);
      setThinkingType(NONE);
      setEffort(NONE);
      setPoolExclude(true);
    }
  }

  function selectCredential(value: string) {
    setCredentialID(value);
    const credential = credentials.find((item) => String(item.id) === value);
    if (credential) setFormat(MANAGED_NATIVE[credential.provider].format);
    setModel("");
    setModels([]);
    setModelsOpen(false);
    setThinkingType(NONE);
    setEffort(NONE);
    setMaxTokens("0");
    setMaxTokensField(NONE);
  }

  // Case-insensitive substring filter, so a provider prefix ("deepseek/") or a
  // fragment ("flash") is enough to narrow a 400-entry list. Native catalog
  // entries match both their display names and protocol model IDs.
  const filteredModels = React.useMemo(() => {
    const q = modelQuery.trim().toLowerCase();
    if (!q) return models;
    return models.filter((entry) => {
      const id = typeof entry === "string" ? entry : entry.id;
      const label = typeof entry === "string" ? "" : entry.name;
      return id.toLowerCase().includes(q) || label.toLowerCase().includes(q);
    });
  }, [models, modelQuery]);

  async function loadModels() {
    if (loadingModels) return;
    if (authMode === "oauth" && !hasCredentialSelection) {
      toast.error("Select a connected account before loading the native catalog");
      return;
    }
    setLoadingModels(true);
    setModels([]);
    setModelQuery("");
    try {
      const r = await api.fetchLLMModels({
        provider: format,
        auth_mode: authMode,
        ...(authMode === "oauth"
          ? storedNativeSelection
            ? { profile_id: profileId }
            : { credential_id: credentialIDNumber }
          : { base_url: baseUrl, api_key: apiKey, proxy, profile_id: profileId }),
      });
      if (r.ok && r.models && r.models.length > 0) {
        setModels(r.models);
        setModelsOpen(true);
        toast.success(
          authMode === "oauth"
            ? `Loaded ${r.models.length} pinned catalog candidates`
            : `Loaded ${r.models.length} models`,
        );
      } else {
        toast.error(`Failed to load models: ${r.error ?? "no models returned"}`);
      }
    } catch (e) {
      toast.error(`Error loading models: ${(e as Error).message}`);
    } finally {
      setLoadingModels(false);
    }
  }

  async function testConnection() {
    if (testing) return;
    if (authMode === "oauth" && !hasCredentialSelection) {
      toast.error("Select an account before testing this profile");
      return;
    }
    setTesting(true);
    try {
      const r = await api.testLLM({
        provider: format,
        model,
        auth_mode: authMode,
        ...(authMode === "oauth"
          ? storedNativeSelection
            ? { profile_id: profileId }
            : { credential_id: credentialIDNumber }
          : {
              profile_id: profileId,
              base_url: baseUrl,
              api_key: apiKey,
              session_header_key: sessionHeaderKey.trim(),
              user_agent: userAgent.trim(),
            }),
        proxy,
        thinking_type: toStore(thinkingType),
        reasoning_effort: toStore(effort),
        streaming,
      });
      if (r.ok)
        toast.success(`Model check passed · ${r.latency_ms ?? "?"}ms · ${r.model ?? model}`, {
          description: r.reply ? `Reply: ${r.reply}` : undefined,
        });
      else toast.error(`Connection failed: ${r.error ?? "unknown"}`);
    } catch (e) {
      toast.error(`Test error: ${(e as Error).message}`);
    } finally {
      setTesting(false);
    }
  }

  async function save() {
    if (!name.trim() || !model.trim()) {
      toast.error("Please fill in a name and a model");
      return;
    }
    if (authMode === "oauth" && !hasCredentialSelection) {
      toast.error("Select a connected account for this native profile");
      return;
    }
    if (!supportsThinkingDisable && thinkingType === "disabled") {
      toast.error("This native model does not support turning reasoning fully off");
      return;
    }
    if (saving) return;
    setSaving(true);
    try {
      const native = authMode === "oauth";
      const { id } = await api.saveLLMProfile({
        ...(profile ? { id: Number(profile.id) } : {}),
        auth_mode: authMode,
        credential_id: native ? credentialIDNumber : profile?.auth_mode === "oauth" ? null : undefined,
        name: name.trim(),
        format,
        model: model.trim(),
        base_url: native ? "" : baseUrl.trim(),
        proxy: proxy.trim(),
        api_key: native ? "" : apiKey,
        rate_per_second: Number(rps) || 0,
        rate_per_minute: Number(rpm) || 0,
        context_window_k: Number(cw) || 0,
        thinking_type: toStore(thinkingType),
        reasoning_effort: toStore(effort),
        priority: Number(priority) || 0,
        pool_exclude: poolExclude,
        streaming,
        max_tokens: outputCapSupported ? Math.max(0, Number(maxTokens) || 0) : 0,
        max_tokens_field: !native && format === "openai" ? toStore(maxTokensField) : "",
        session_header_key: native ? "" : sessionHeaderKey.trim(),
        user_agent: native ? "" : userAgent.trim(),
        price_input_per_m: native ? 0 : profile?.price_input_per_m,
        price_output_per_m: native ? 0 : profile?.price_output_per_m,
        price_cache_read_per_m: native ? 0 : profile?.price_cache_read_per_m,
        price_cache_write_per_m: native ? 0 : profile?.price_cache_write_per_m,
        retry,
      });
      if (isNew) toast.success(`Created: ${name.trim()} (use "Set as active" on its card to enable it)`);
      else
        toast.success(
          profile?.is_default ? "Saved; the active profile takes effect immediately, no restart needed" : "Saved",
        );
      onSaved(String(id));
      onOpenChange(false);
    } catch (e) {
      toast.error(`Save failed: ${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  let keyPlaceholder = "sk-…";
  if (format === "ollama") {
    keyPlaceholder = "optional — a local Ollama server runs keyless";
  } else if (keyHint) {
    keyPlaceholder = `Set (${keyHint}); leave empty to keep it`;
  }

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent
        side="right"
        className="flex w-full flex-col gap-0 p-0 data-[side=right]:sm:min-w-[420px] data-[side=right]:sm:max-w-xl"
      >
        <SheetHeader className="px-4">
          <SheetTitle className="flex items-center gap-2">
            {isNew ? "New model profile" : `Edit: ${profile?.name}`}
            {profile?.is_default && (
              <Badge variant="outline" className="border-amber-400/50 text-amber-500">
                Active
              </Badge>
            )}
          </SheetTitle>
          <SheetDescription>
            {isNew
              ? "New profiles are not activated automatically; use 'Set as active' on its card to enable it."
              : "Click save after editing; saving the active profile takes effect immediately for all agents."}
          </SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-4 pb-4">
          <div className="grid gap-4 rounded-lg border p-3 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="p-auth-mode">Authentication</Label>
              <Select value={authMode} onValueChange={(value) => changeAuthMode(value as LLMAuthMode)}>
                <SelectTrigger id="p-auth-mode">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="api_key">API key / local server</SelectItem>
                  <SelectItem value="oauth">Experimental native account</SelectItem>
                </SelectContent>
              </Select>
              <p className="text-muted-foreground text-xs">
                {authMode === "oauth"
                  ? "Uses a backend-stored browser session. Tokens are never entered or exposed here."
                  : "Preserves the existing API-key and keyless Ollama workflow."}
              </p>
            </div>
            {authMode === "oauth" && (
              <div className="grid gap-2">
                <Label htmlFor="p-credential">Account</Label>
                <Select value={credentialID} onValueChange={selectCredential}>
                  <SelectTrigger id="p-credential">
                    <SelectValue placeholder="Select a connected account" />
                  </SelectTrigger>
                  <SelectContent>
                    {credentials.map((credential) => (
                      <SelectItem key={credential.id} value={String(credential.id)}>
                        {credential.provider === "anthropic-oauth" ? "Claude" : "ChatGPT Codex"} · {credential.label} ·{" "}
                        {credential.status === "ready" ? "session stored" : credential.status.replaceAll("_", " ")}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {credentialsError ? (
                  <p className="text-destructive text-xs">{credentialsError}</p>
                ) : credentials.length === 0 ? (
                  <p className="text-muted-foreground text-xs">
                    Connect an account in the Accounts tab before creating a native profile.
                  </p>
                ) : (
                  <p className="text-muted-foreground text-xs">
                    Selection is explicit. A disconnected account remains selected so it can be reauthenticated in
                    place.
                  </p>
                )}
              </div>
            )}
          </div>

          <div className="grid gap-4 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="p-name">Name</Label>
              <Input
                id="p-name"
                placeholder="e.g. OpenAI production"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="p-format">Format</Label>
              <Select
                value={format}
                onValueChange={(v) => setFormat(v as ProfileFormat)}
                disabled={authMode === "oauth"}
              >
                <SelectTrigger id="p-format">
                  <SelectValue placeholder="Select a format" />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="anthropic">Anthropic</SelectItem>
                  <SelectItem value="openai">OpenAI (Chat Completions)</SelectItem>
                  <SelectItem value="openai-responses">OpenAI (Responses API)</SelectItem>
                  <SelectItem value="ollama">Ollama (local, no key needed)</SelectItem>
                </SelectContent>
              </Select>
              {authMode === "oauth" && (
                <p className="text-muted-foreground text-xs">Fixed by the selected account provider.</p>
              )}
            </div>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="p-model">Model</Label>
            <div className="flex gap-2">
              <Input
                id="p-model"
                className="font-mono"
                placeholder={format === "ollama" ? "llama3.2" : "claude-opus-4-8"}
                value={model}
                onChange={(e) => setModel(e.target.value)}
              />
              {/* modal: this Popover's content is portaled to <body>, outside the Sheet's scroll lock,
                  so without modal the list renders but cannot scroll. modal lets it hold the topmost scroll lock itself. */}
              <Popover
                open={modelsOpen}
                onOpenChange={(o) => {
                  setModelsOpen(o);
                  if (!o) setModelQuery("");
                }}
                modal
              >
                <PopoverTrigger asChild>
                  <Button
                    type="button"
                    variant="outline"
                    size="icon"
                    className="shrink-0"
                    disabled={loadingModels}
                    onClick={loadModels}
                    title={authMode === "oauth" ? "Load pinned model catalog" : "Load available models from the API"}
                    aria-label={authMode === "oauth" ? "Load pinned model catalog" : "Load available models"}
                  >
                    {loadingModels ? <Loader2Icon className="animate-spin" /> : <RefreshCwIcon />}
                  </Button>
                </PopoverTrigger>
                {models.length > 0 && (
                  <PopoverContent className="w-80 gap-0 p-0" align="end">
                    <div className="border-b p-1">
                      <Input
                        autoFocus
                        className="h-8 border-0 shadow-none focus-visible:ring-0 dark:bg-transparent"
                        placeholder={`Search ${models.length} models…`}
                        value={modelQuery}
                        onChange={(e) => setModelQuery(e.target.value)}
                        spellCheck={false}
                        aria-label="Search models"
                      />
                    </div>
                    <div className="max-h-64 overflow-y-auto overscroll-contain p-1">
                      {filteredModels.length === 0 ? (
                        <p className="px-2 py-6 text-center text-muted-foreground text-xs">
                          No model matches “{modelQuery.trim()}”
                        </p>
                      ) : (
                        filteredModels.map((entry) => {
                          const id = typeof entry === "string" ? entry : entry.id;
                          const label = typeof entry === "string" ? entry : entry.name;
                          return (
                            <button
                              key={id}
                              type="button"
                              className="w-full shrink-0 rounded-md px-2 py-1.5 text-left text-xs hover:bg-accent hover:text-accent-foreground"
                              onClick={() => {
                                setModel(id);
                                setModelsOpen(false);
                                if (typeof entry !== "string") {
                                  setCw(String(entry.context_window_k));
                                  if (!entry.supports_thinking_disable && thinkingType === "disabled") {
                                    setThinkingType(NONE);
                                  }
                                  if (!entry.supports_output_cap) {
                                    setMaxTokens("0");
                                    setMaxTokensField(NONE);
                                  }
                                  if (
                                    entry.reasoning_efforts?.length &&
                                    effort !== NONE &&
                                    !entry.reasoning_efforts.includes(effort)
                                  ) {
                                    setEffort(NONE);
                                  }
                                }
                              }}
                            >
                              <span className="block font-medium">{label}</span>
                              {typeof entry !== "string" && (
                                <code className="block truncate text-muted-foreground">{id}</code>
                              )}
                            </button>
                          );
                        })
                      )}
                    </div>
                  </PopoverContent>
                )}
              </Popover>
            </div>
            {authMode === "oauth" && (
              <p className="text-muted-foreground text-xs">
                The pinned catalog describes protocol capabilities only. It does not confirm this account&apos;s model
                entitlement, quota, balance, or billing.
              </p>
            )}
            {selectedCatalog && (
              <div className="flex flex-wrap gap-x-3 gap-y-1 text-muted-foreground text-xs">
                <span>{selectedCatalog.context_window_k}K context</span>
                <span>Up to {selectedCatalog.max_output_tokens.toLocaleString()} output tokens</span>
                <span>{selectedCatalog.supports_tools ? "Tools supported" : "No tool support"}</span>
                <span>
                  {selectedCatalog.supports_output_cap ? "Output cap supported" : "Operator output cap unsupported"}
                </span>
              </div>
            )}
          </div>

          {authMode === "oauth" ? (
            <div className="grid gap-3 rounded-lg border p-3">
              <div className="grid gap-1">
                <Label className="text-sm">Managed endpoint</Label>
                <code className="break-all text-muted-foreground text-xs">
                  {managedNative?.endpoint ?? "Select an account to view its fixed endpoint"}
                </code>
              </div>
              <div className="grid gap-1 border-t pt-3">
                <Label className="text-sm">Managed User-Agent</Label>
                <code className="break-all text-muted-foreground text-xs">
                  {managedNative?.userAgent ?? "Select an account"}
                </code>
              </div>
              <p className="text-muted-foreground text-xs">
                These values are fixed by the native transport and cannot be redirected or overridden by this profile.
              </p>
            </div>
          ) : (
            <div className="grid gap-2">
              <Label htmlFor="p-base-url">Base URL (optional)</Label>
              <Input
                id="p-base-url"
                className="font-mono"
                placeholder={format === "ollama" ? OLLAMA_DEFAULT_BASE_URL : "https://api.openai.com/v1"}
                value={baseUrl}
                onChange={(e) => setBaseUrl(e.target.value)}
              />
            </div>
          )}

          <div className="grid gap-2">
            <Label htmlFor="p-proxy">Proxy (optional)</Label>
            <Input
              id="p-proxy"
              className="font-mono"
              placeholder="socks5://user:pass@127.0.0.1:1080 · http://127.0.0.1:8080"
              value={proxy}
              onChange={(e) => setProxy(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">
              Only outbound LLM requests go through this proxy; http/https/socks5 are supported, optionally with
              credentials (e.g. socks5://user:pass@host:port — URL-encode the password if it contains special
              characters). Leave empty for no proxy (direct connection).
            </p>
          </div>

          {authMode === "api_key" && (
            <>
              <div className="grid gap-2">
                <Label htmlFor="p-session-header">Custom session header (optional)</Label>
                <Input
                  id="p-session-header"
                  className="font-mono"
                  placeholder="e.g. x-session-id (empty = not sent)"
                  value={sessionHeaderKey}
                  onChange={(e) => setSessionHeaderKey(e.target.value)}
                />
                <p className="text-muted-foreground text-xs">
                  Once a header name is set, every request carries this HTTP header, with the value filled in
                  automatically as the <b>current session's session id</b> (chat sessions like conv-12, workers like
                  exp3-worker-i87). Meant for gateways that do prompt caching / sticky routing on a session-id header;
                  stable across turns of one session, distinct across sessions. Leave empty to not send it.
                </p>
              </div>

              <div className="grid gap-2">
                <Label htmlFor="p-user-agent">Custom User-Agent (optional)</Label>
                <Input
                  id="p-user-agent"
                  className="font-mono"
                  placeholder="e.g. my-app/1.0 (empty = default UA)"
                  value={userAgent}
                  onChange={(e) => setUserAgent(e.target.value)}
                />
                <p className="text-muted-foreground text-xs">
                  Overrides the <b>User-Agent</b> header on every outbound LLM request. Useful for gateways that route
                  or allow-list by User-Agent. Leave empty to keep the default User-Agent unchanged.
                </p>
              </div>

              <div className="grid gap-2">
                <Label htmlFor="p-api-key">{format === "ollama" ? "API Key (optional for Ollama)" : "API Key"}</Label>
                <Input
                  id="p-api-key"
                  type="password"
                  placeholder={keyPlaceholder}
                  value={apiKey}
                  onChange={(e) => setApiKey(e.target.value)}
                  // A masked field with no autocomplete hint looks like this site's
                  // login password: password managers fill it and Save persists the
                  // wrong secret over the stored key. new-password keeps them out.
                  autoComplete="new-password"
                  spellCheck={false}
                  data-1p-ignore="true"
                  data-lpignore="true"
                />
              </div>
            </>
          )}

          <div className="grid gap-4 sm:grid-cols-3">
            <div className="grid gap-2">
              <Label htmlFor="p-rps">Rate limit per second</Label>
              <Input id="p-rps" type="number" min={0} value={rps} onChange={(e) => setRps(e.target.value)} />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="p-rpm">Rate limit per minute</Label>
              <Input id="p-rpm" type="number" min={0} value={rpm} onChange={(e) => setRpm(e.target.value)} />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="p-cw">
                {authMode === "oauth" ? "Context window (K) · catalog" : "Context window (K)"}
              </Label>
              <Input
                id="p-cw"
                type="number"
                min={0}
                max={1000}
                disabled={authMode === "oauth"}
                value={cw}
                onChange={(e) => setCw(e.target.value)}
                placeholder="200"
              />
            </div>
          </div>
          <p className="-mt-2 text-muted-foreground text-xs">
            {authMode === "oauth"
              ? "Rate limits of 0 mean unlimited within ARTEX. The context window is pinned by the selected catalog model and is not an account quota result."
              : "Rate limits of 0 mean unlimited, shared across all agents. The context window is in K (thousand tokens); 0 = the 200K default, capped at 1000 (1M). Setting it too high keeps compression from triggering."}
          </p>

          <div className="grid gap-3 rounded-lg border p-3">
            <div className="flex items-center justify-between gap-4">
              <div className="grid gap-0.5">
                <Label htmlFor="p-priority" className="text-sm">
                  Pool priority
                </Label>
                <p className="text-muted-foreground text-xs">
                  Higher numbers are picked first; the active profile is always first regardless of this value. Profiles
                  with equal priority take turns leading, spreading quota naturally.
                </p>
              </div>
              <Input
                id="p-priority"
                type="number"
                className="w-24 shrink-0"
                value={priority}
                onChange={(e) => setPriority(e.target.value)}
              />
            </div>
            <div className="flex items-center justify-between gap-4 border-t pt-3">
              <div className="grid gap-0.5">
                <Label className="text-sm">Excluded from pool</Label>
                <p className="text-muted-foreground text-xs">
                  When enabled, this profile is never used as a failover target (Agents / tasks can still pin it
                  explicitly). Suited to expensive profiles reserved for one specific Agent that should not be burned
                  through by other agents' failures.
                </p>
              </div>
              <Switch checked={poolExclude} onCheckedChange={setPoolExclude} aria-label="Excluded from pool" />
            </div>
            <div className="flex items-center justify-between gap-4 border-t pt-3">
              <div className="grid gap-0.5">
                <Label className="text-sm">Streaming output</Label>
                <p className="text-muted-foreground text-xs">
                  {selectedNativeProvider === "openai-codex-oauth"
                    ? "On streams live progress from Codex. Off buffers the final message for the caller, but the upstream Codex request still uses stream:true and ARTEX collects its SSE before returning. Native failures remain terminal and are not replayed."
                    : "On (default) streams over SSE, with live in-flight progress and real-time token counting. Off uses true non-streaming (stream:false, returning the full response in one shot) — a way around gateways with broken SSE implementations (empty frames / dropped thinking-field frames), at the cost of losing live in-flight progress."}
                </p>
              </div>
              <Switch checked={streaming} onCheckedChange={setStreaming} aria-label="Streaming output" />
            </div>
          </div>

          <div className="grid gap-3 rounded-lg border p-3">
            <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
              <div className="grid gap-0.5">
                <Label htmlFor="p-max-tokens" className="text-sm">
                  Output cap · max tokens
                </Label>
                <p className="text-muted-foreground text-xs">
                  {authMode === "oauth" && !outputCapSupported
                    ? selectedNativeProvider === "openai-codex-oauth"
                      ? "The Codex native transport does not support an operator output cap. Internal auxiliary budgets are advisory and are not a wire-level cap."
                      : "Load and select a pinned catalog model to confirm whether its native transport supports an output cap."
                    : "How many tokens one reply may generate at most. 0 leaves the field unsent and uses the provider default; the context window is a separate local compression setting."}
                </p>
              </div>
              <Input
                id="p-max-tokens"
                type="number"
                min={0}
                className="w-full sm:w-28 sm:shrink-0"
                value={outputCapSupported ? maxTokens : "0"}
                disabled={!outputCapSupported}
                onChange={(e) => setMaxTokens(e.target.value)}
                placeholder="0"
              />
            </div>
            <div className="flex flex-col gap-3 border-t pt-3 sm:flex-row sm:items-center sm:justify-between">
              <div className="grid gap-0.5">
                <Label className="text-sm">Cap field name</Label>
                <p className="text-muted-foreground text-xs">
                  {authMode === "oauth"
                    ? "The native transport uses its fixed protocol field when the selected model supports this control."
                    : MAX_TOKENS_FIELD_HINTS[format]}
                </p>
              </div>
              <Select
                value={authMode === "api_key" && format === "openai" ? maxTokensField : NONE}
                onValueChange={setMaxTokensField}
                disabled={authMode === "oauth" || format !== "openai"}
              >
                <SelectTrigger className="w-full sm:w-56 sm:shrink-0">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {MAX_TOKENS_FIELDS.map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="grid gap-3 rounded-lg border p-3">
            <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
              <div className="grid gap-0.5">
                <Label className="text-sm">Thinking switch · thinking.type</Label>
                <p className="text-muted-foreground text-xs">
                  {!supportsThinkingDisable
                    ? selectedCatalog || selectedNativeProvider === "openai-codex-oauth"
                      ? "This native model does not expose true reasoning-off. “Not sent” keeps the provider default; choosing a lower effort only minimizes reasoning and is not equivalent to off."
                      : "Load and select a pinned catalog model before choosing a thinking-disable control."
                    : "Controls whether the thinking field is omitted, disabled, or enabled. It remains independent from the effort setting below."}
                </p>
              </div>
              <Select value={thinkingType} onValueChange={setThinkingType}>
                <SelectTrigger className="w-full sm:w-40 sm:shrink-0">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {thinkingOptions.map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="flex flex-col gap-3 border-t pt-3 sm:flex-row sm:items-center sm:justify-between">
              <div className="grid gap-0.5">
                <Label className="text-sm">Thinking effort · reasoning_effort</Label>
                <p className="text-muted-foreground text-xs">
                  A separate effort control. The pinned native catalog limits this list to protocol-supported values;
                  availability is still checked by the provider.
                </p>
              </div>
              <Select value={effort} onValueChange={setEffort}>
                <SelectTrigger className="w-full sm:w-40 sm:shrink-0">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {effortOptions.map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          {authMode === "api_key" ? (
            <ProfileRetryFields value={retry} onChange={setRetry} />
          ) : (
            <div className="rounded-lg border border-dashed p-3 text-muted-foreground text-xs">
              Native account failures are terminal: ARTEX does not replay the physical request, rotate accounts, or fall
              back to an API key.
            </div>
          )}
        </div>

        <div className="flex gap-2 border-t px-4 py-3">
          <Button variant="outline" onClick={testConnection} disabled={testing}>
            {testing ? <Loader2Icon className="animate-spin" /> : <PlugZapIcon />}
            {testing ? "Testing…" : "Test connection"}
          </Button>
          <Button onClick={save} disabled={saving} className="flex-1">
            {saving && <Loader2Icon className="animate-spin" />}
            {!saving && (isNew ? <PlusIcon /> : <SaveIcon />)}
            {isNew ? "Create" : "Save"}
          </Button>
        </div>
      </SheetContent>
    </Sheet>
  );
}

// ─────────────────────────────────────────────────────────────────────────────

export default function LLMPage() {
  const [profiles, setProfiles] = React.useState<LLMProfile[]>([]);
  const [profilesLoading, setProfilesLoading] = React.useState(true);
  const [profilesError, setProfilesError] = React.useState("");
  const [credentials, setCredentials] = React.useState<LLMCredential[]>([]);
  const [credentialsLoading, setCredentialsLoading] = React.useState(true);
  const [credentialsError, setCredentialsError] = React.useState("");
  const [pool, setPool] = React.useState<LLMPoolStatus | null>(null);
  const [poolOpen, setPoolOpen] = React.useState(false);
  // The drawer's open flag and its content are stored separately: editing stays put on close, otherwise during the closing animation the title would flash
  // from "Edit X" to "New". editing = null means create.
  const [editOpen, setEditOpen] = React.useState(false);
  const [editing, setEditing] = React.useState<LLMProfile | null>(null);
  const openEditor = React.useCallback((p: LLMProfile | null) => {
    setEditing(p);
    setEditOpen(true);
  }, []);

  const loadPool = React.useCallback(async () => {
    try {
      setPool(await api.llmPool());
    } catch {
      /* Pool state is supplementary; profile and account errors are surfaced independently. */
    }
  }, []);

  const loadProfiles = React.useCallback(async () => {
    setProfilesLoading(true);
    setProfilesError("");
    try {
      setProfiles(await api.llmProfiles());
    } catch (cause) {
      setProfilesError((cause as Error).message);
    } finally {
      setProfilesLoading(false);
    }
  }, []);

  const loadCredentials = React.useCallback(async () => {
    setCredentialsLoading(true);
    setCredentialsError("");
    try {
      setCredentials(await api.llmCredentials());
    } catch (cause) {
      setCredentialsError((cause as Error).message);
    } finally {
      setCredentialsLoading(false);
    }
  }, []);

  const load = React.useCallback(async () => {
    await Promise.all([loadProfiles(), loadPool(), loadCredentials()]);
  }, [loadCredentials, loadPool, loadProfiles]);

  React.useEffect(() => {
    void load();
  }, [load]);

  // The health badge on each card reads pool state by profile id.
  const health = React.useMemo(() => {
    const m = new Map<string, LLMPoolMember>();
    for (const c of pool?.chain ?? []) m.set(c.profile_id, c);
    return m;
  }, [pool]);
  const credentialsByID = React.useMemo(() => {
    const result = new Map<number, LLMCredential>();
    for (const credential of credentials) result.set(credential.id, credential);
    return result;
  }, [credentials]);

  async function activate(id: string, name: string) {
    try {
      await api.activateLLMProfile(id);
      toast.success(`Activated: ${name}`);
      await load();
    } catch (e) {
      toast.error(`Activation failed: ${(e as Error).message}`);
    }
  }

  async function remove(p: LLMProfile) {
    if (p.is_default) {
      toast.error("Cannot delete the active profile");
      return;
    }
    try {
      await api.deleteLLMProfile(p.id);
      toast.success(`Deleted: ${p.name}`);
      await load();
    } catch (e) {
      toast.error(`Delete failed: ${(e as Error).message}`);
    }
  }

  const poolOn = pool?.enabled ?? false;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="font-semibold text-xl tracking-tight">LLM</h1>
          <p className="max-w-3xl text-muted-foreground text-sm">
            Model profiles, connected accounts, and rate limits shared by all agents. Click a profile to edit it; new
            profiles require explicit activation.
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Button size="sm" variant="outline" onClick={() => setPoolOpen(true)}>
            <ZapIcon /> Pool settings
            {poolOn && (
              <Badge variant="outline" className="ml-1 border-emerald-500/50 text-emerald-600 dark:text-emerald-400">
                On
              </Badge>
            )}
          </Button>
          <Button size="sm" variant="outline" onClick={() => openEditor(null)}>
            <PlusIcon /> Create
          </Button>
        </div>
      </div>

      <Tabs defaultValue="profiles" className="flex-1">
        <TabsList className="max-w-full overflow-x-auto">
          <TabsTrigger value="profiles">Model profiles</TabsTrigger>
          <TabsTrigger value="accounts">Accounts</TabsTrigger>
          <TabsTrigger value="retry">Retry & backoff</TabsTrigger>
        </TabsList>

        <TabsContent value="profiles" className="mt-4">
          {profilesError && (
            <Alert variant="destructive" className="mb-4">
              <AlertCircleIcon />
              <AlertTitle>Model profiles unavailable</AlertTitle>
              <AlertDescription className="grid gap-2">
                <span>{profilesError}</span>
                <Button type="button" size="sm" variant="outline" className="w-fit" onClick={() => void loadProfiles()}>
                  Retry profiles
                </Button>
              </AlertDescription>
            </Alert>
          )}
          {credentialsError && (
            <Alert className="mb-4">
              <AlertCircleIcon />
              <AlertTitle>Connected account status unavailable</AlertTitle>
              <AlertDescription>
                {credentialsError} API-key and Ollama profiles remain available; native account state cannot be
                confirmed until the account API recovers.
              </AlertDescription>
            </Alert>
          )}
          <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
            {profiles.map((p) => {
              const credential = p.credential_id ? credentialsByID.get(p.credential_id) : undefined;
              const h = healthOf(p, health.get(p.id), credential);
              return (
                // biome-ignore lint/a11y/useSemanticElements: the card contains its own action buttons; a native <button> would nest buttons (invalid HTML)
                <Card
                  key={p.id}
                  role="button"
                  tabIndex={0}
                  onClick={() => openEditor(p)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      openEditor(p);
                    }
                  }}
                  className={cn(
                    "cursor-pointer gap-0 py-4 outline-none transition-colors hover:border-foreground/30 focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
                    p.is_default && "border-amber-400/50 bg-amber-400/5",
                  )}
                >
                  <CardContent className="grid gap-2 px-4">
                    <div className="flex items-start gap-2">
                      <StarIcon
                        className={cn(
                          "mt-0.5 size-4 shrink-0",
                          p.is_default ? "fill-amber-400 text-amber-400" : "text-muted-foreground",
                        )}
                      />
                      <div className="min-w-0 flex-1">
                        <div className="flex flex-wrap items-center gap-2">
                          <span className="truncate font-medium text-sm">{p.name}</span>
                          <Badge variant="outline" className="uppercase">
                            {p.format}
                          </Badge>
                          <Badge variant="outline" className={cn("ml-auto", h.cls)} title={h.hint}>
                            {h.label}
                          </Badge>
                        </div>
                        <code className="mt-1 block truncate font-mono text-muted-foreground text-xs">{p.model}</code>
                      </div>
                    </div>

                    <div className="flex flex-wrap gap-x-3 gap-y-0.5 pl-6 text-muted-foreground text-xs">
                      {(p.auth_mode ?? "api_key") === "oauth" ? (
                        <>
                          <span>Native account</span>
                          {credential && <span>{credential.label}</span>}
                          {p.auth_binding_generation !== undefined && <span>Binding {p.auth_binding_generation}</span>}
                        </>
                      ) : (
                        p.api_key_hint && <span>{p.api_key_hint}</span>
                      )}
                      <span>
                        {p.rate_per_second}/s · {p.rate_per_minute}/min
                      </span>
                      {p.proxy && <span className="truncate">Proxy {p.proxy}</span>}
                      {p.reasoning_effort && <span>Thinking effort {p.reasoning_effort}</span>}
                      {poolOn &&
                        !p.is_default &&
                        (p.pool_exclude ? <span>Excluded from pool</span> : <span>Priority {p.priority ?? 0}</span>)}
                    </div>

                    <div className="mt-1 flex gap-2">
                      <Button
                        size="sm"
                        variant="outline"
                        className="flex-1"
                        disabled={p.is_default}
                        onClick={(e) => {
                          e.stopPropagation();
                          void activate(p.id, p.name);
                        }}
                      >
                        {p.is_default ? "Active" : "Set as active"}
                      </Button>
                      <Button
                        size="icon"
                        variant="outline"
                        aria-label="Delete profile"
                        onClick={(e) => {
                          e.stopPropagation();
                          void remove(p);
                        }}
                      >
                        <Trash2Icon className="text-destructive" />
                      </Button>
                    </div>
                  </CardContent>
                </Card>
              );
            })}
            {profilesLoading && profiles.length === 0 && (
              <div className="col-span-full flex items-center justify-center gap-2 rounded-lg border border-dashed p-10 text-muted-foreground text-sm">
                <Loader2Icon className="size-4 animate-spin" /> Loading model profiles…
              </div>
            )}
            {!profilesLoading && !profilesError && profiles.length === 0 && (
              <div className="col-span-full rounded-lg border border-dashed p-10 text-center text-muted-foreground text-sm">
                No model profiles yet — click “Create” in the top right to add the first one.
              </div>
            )}
          </div>
        </TabsContent>

        <TabsContent value="accounts" className="mt-4">
          <AccountsPanel
            credentials={credentials}
            loading={credentialsLoading}
            error={credentialsError}
            onRefresh={loadCredentials}
          />
        </TabsContent>

        <TabsContent value="retry" className="mt-4">
          <RetryPolicyPanel />
        </TabsContent>
      </Tabs>

      <ProfileSheet
        profile={editing}
        credentials={credentials}
        credentialsError={credentialsError}
        open={editOpen}
        onOpenChange={setEditOpen}
        onSaved={() => void load()}
      />
      <PoolSheet open={poolOpen} onOpenChange={setPoolOpen} pool={pool} onReload={loadPool} />
    </div>
  );
}
