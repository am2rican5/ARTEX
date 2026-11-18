"use client";

import * as React from "react";

import {
  AlertCircleIcon,
  CheckCircle2Icon,
  ExternalLinkIcon,
  KeyRoundIcon,
  Loader2Icon,
  LogOutIcon,
  RefreshCwIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { LLMCredential, LLMCredentialAttempt, LLMCredentialProvider } from "@/lib/types";

const POLL_INTERVAL_MS = 1_500;
const MAX_POLL_REQUESTS = 80;

const PROVIDERS: Array<{
  id: LLMCredentialProvider;
  name: string;
  description: string;
}> = [
  {
    id: "anthropic-oauth",
    name: "Claude account",
    description:
      "Experimental direct Claude browser authorization. This is not an official Anthropic endorsement, and availability remains subject to Anthropic's terms and account policy.",
  },
  {
    id: "openai-codex-oauth",
    name: "ChatGPT Codex account",
    description:
      "Experimental legacy Codex browser OAuth with PKCE. This is not Sign in with ChatGPT (SIWC) and does not imply an OpenAI endorsement.",
  },
];

function providerName(provider: LLMCredentialProvider) {
  return PROVIDERS.find((item) => item.id === provider)?.name ?? provider;
}

function statusLabel(status: LLMCredential["status"]) {
  switch (status) {
    case "ready":
      return "Session stored";
    case "reauth_required":
      return "Reauthentication required";
    case "revoked":
      return "Disconnected";
  }
}

function statusClass(status: LLMCredential["status"]) {
  switch (status) {
    case "ready":
      return "border-emerald-500/50 text-emerald-600 dark:text-emerald-400";
    case "reauth_required":
      return "border-amber-500/50 text-amber-600 dark:text-amber-400";
    case "revoked":
      return "border-muted-foreground/40 text-muted-foreground";
  }
}

function formatTime(value: string) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

function waitForPoll(signal: AbortSignal) {
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  const timer = window.setTimeout(resolve, POLL_INTERVAL_MS);
  signal.addEventListener(
    "abort",
    () => {
      window.clearTimeout(timer);
      reject(new DOMException("Aborted", "AbortError"));
    },
    { once: true },
  );
  return promise;
}

export function AccountsPanel({
  credentials,
  loading,
  error,
  onRefresh,
}: {
  credentials: LLMCredential[];
  loading: boolean;
  error: string;
  onRefresh: () => Promise<void>;
}) {
  const [attempt, setAttempt] = React.useState<LLMCredentialAttempt | null>(null);
  const [redirectURL, setRedirectURL] = React.useState("");
  const [flowError, setFlowError] = React.useState("");
  const [notice, setNotice] = React.useState("");
  const [startingProvider, setStartingProvider] = React.useState<LLMCredentialProvider | null>(null);
  const [reauthenticatingID, setReauthenticatingID] = React.useState<number | null>(null);
  const [disconnectingID, setDisconnectingID] = React.useState<number | null>(null);
  const [submitting, setSubmitting] = React.useState(false);
  const [refreshingAttempt, setRefreshingAttempt] = React.useState(false);
  const pollController = React.useRef<AbortController | null>(null);

  const stopPolling = React.useCallback(() => {
    pollController.current?.abort();
    pollController.current = null;
  }, []);

  React.useEffect(() => stopPolling, [stopPolling]);

  const finishAttempt = React.useCallback(
    async (next: LLMCredentialAttempt) => {
      setAttempt(next);
      if (next.status === "complete") {
        stopPolling();
        setRedirectURL("");
        setFlowError("");
        toast.success(next.credential_id ? "Account session stored" : "Account connected");
        await onRefresh();
      } else if (next.status === "failed") {
        stopPolling();
        setFlowError(next.error || "Authorization failed. Start a new attempt to try again.");
      }
    },
    [onRefresh, stopPolling],
  );

  const pollAttempt = React.useCallback(
    (initial: LLMCredentialAttempt) => {
      stopPolling();
      if (initial.status !== "pending") return;
      const controller = new AbortController();
      pollController.current = controller;

      void (async () => {
        const expiresAt = Date.parse(initial.expires_at);
        try {
          for (let count = 0; count < MAX_POLL_REQUESTS; count += 1) {
            if (Number.isFinite(expiresAt) && Date.now() >= expiresAt) {
              setFlowError("This authorization attempt expired. Start a new attempt to continue.");
              return;
            }
            await waitForPoll(controller.signal);
            const next = await api.llmCredentialAttempt(initial.id, controller.signal);
            setAttempt(next);
            if (next.status !== "pending") {
              await finishAttempt(next);
              return;
            }
          }
          setFlowError("Authorization is still pending. Use “Refresh status” to check again or start a new attempt.");
        } catch (cause) {
          if ((cause as Error).name !== "AbortError") {
            setFlowError(`Could not check authorization status: ${(cause as Error).message}`);
          }
        } finally {
          if (pollController.current === controller) pollController.current = null;
        }
      })();
    },
    [finishAttempt, stopPolling],
  );

  async function begin(provider: LLMCredentialProvider, credentialID?: number) {
    stopPolling();
    setFlowError("");
    setNotice("");
    setRedirectURL("");
    if (credentialID) setReauthenticatingID(credentialID);
    else setStartingProvider(provider);
    try {
      const next = await api.loginLLMCredential(provider, credentialID);
      setAttempt(next);
      if (next.status === "pending") pollAttempt(next);
      else await finishAttempt(next);
    } catch (cause) {
      setFlowError(`Could not start authorization: ${(cause as Error).message}`);
    } finally {
      setStartingProvider(null);
      setReauthenticatingID(null);
    }
  }

  async function refreshAttempt() {
    if (!attempt || refreshingAttempt) return;
    setRefreshingAttempt(true);
    setFlowError("");
    try {
      const next = await api.llmCredentialAttempt(attempt.id);
      setAttempt(next);
      if (next.status === "pending") pollAttempt(next);
      else await finishAttempt(next);
    } catch (cause) {
      setFlowError(`Could not refresh authorization status: ${(cause as Error).message}`);
    } finally {
      setRefreshingAttempt(false);
    }
  }

  async function submitRedirect() {
    if (!attempt || !redirectURL.trim() || submitting) return;
    stopPolling();
    setSubmitting(true);
    setFlowError("");
    try {
      const next = await api.submitLLMCredentialAttempt(attempt.id, redirectURL.trim());
      setRedirectURL("");
      setAttempt(next);
      if (next.status === "pending") pollAttempt(next);
      else await finishAttempt(next);
    } catch (cause) {
      setFlowError(`Could not finish authorization: ${(cause as Error).message}`);
    } finally {
      setSubmitting(false);
    }
  }

  async function disconnect(credential: LLMCredential) {
    if (disconnectingID !== null) return;
    setDisconnectingID(credential.id);
    setNotice("");
    try {
      const result = await api.disconnectLLMCredential(credential.id);
      await onRefresh();
      if (result.remote_revocation_confirmed) {
        setNotice(`${credential.label} was disconnected locally and remote revocation was confirmed.`);
        toast.success("Account disconnected");
      } else {
        const warning = result.warning || "Local access was removed, but remote revocation could not be confirmed.";
        setNotice(warning);
        toast.warning("Disconnected locally; remote revocation unconfirmed");
      }
    } catch (cause) {
      toast.error(`Disconnect failed: ${(cause as Error).message}`);
    } finally {
      setDisconnectingID(null);
    }
  }

  const attemptExpired = attempt?.status === "pending" && Date.now() >= Date.parse(attempt.expires_at);

  return (
    <div className="grid gap-4">
      <div className="grid gap-3 lg:grid-cols-2">
        {PROVIDERS.map((provider) => (
          <Card key={provider.id} className="gap-3 py-4">
            <CardHeader className="gap-1 px-4">
              <div className="flex flex-wrap items-center gap-2">
                <CardTitle className="text-sm">{provider.name}</CardTitle>
                <Badge variant="outline">Experimental</Badge>
              </div>
              <CardDescription>{provider.description}</CardDescription>
            </CardHeader>
            <CardContent className="px-4">
              <Button
                type="button"
                size="sm"
                variant="outline"
                disabled={startingProvider !== null || reauthenticatingID !== null}
                onClick={() => void begin(provider.id)}
              >
                {startingProvider === provider.id ? <Loader2Icon className="animate-spin" /> : <KeyRoundIcon />}
                Connect new account
              </Button>
            </CardContent>
          </Card>
        ))}
      </div>

      {attempt && (
        <Card className="gap-3 py-4" aria-live="polite">
          <CardHeader className="gap-1 px-4">
            <div className="flex flex-wrap items-center gap-2">
              <CardTitle className="text-sm">{providerName(attempt.provider)} authorization</CardTitle>
              <Badge variant="outline">
                {attempt.status === "complete"
                  ? "Complete"
                  : attempt.status === "failed" || attemptExpired
                    ? "Failed"
                    : "Pending"}
              </Badge>
            </div>
            <CardDescription>
              {attempt.status === "pending" && !attemptExpired
                ? `This attempt expires ${formatTime(attempt.expires_at)}. The selected profile and account stay unchanged until the exchange completes.`
                : attempt.status === "complete"
                  ? "The validated account session is stored by the backend. No provider token is exposed to this browser."
                  : attempt.error || "This attempt did not complete."}
            </CardDescription>
          </CardHeader>
          {attempt.status === "pending" && !attemptExpired && (
            <CardContent className="grid gap-4 px-4">
              <div className="flex flex-wrap gap-2">
                {attempt.authorization_url && (
                  <Button asChild size="sm">
                    <a
                      href={attempt.authorization_url}
                      target="_blank"
                      rel="noopener noreferrer"
                      referrerPolicy="no-referrer"
                    >
                      Open authorization page <ExternalLinkIcon />
                    </a>
                  </Button>
                )}
                <Button
                  type="button"
                  size="sm"
                  variant="outline"
                  disabled={refreshingAttempt}
                  onClick={() => void refreshAttempt()}
                >
                  {refreshingAttempt ? <Loader2Icon className="animate-spin" /> : <RefreshCwIcon />}
                  Refresh status
                </Button>
              </div>
              <div className="grid gap-2">
                <Label htmlFor="llm-final-redirect">Manual final redirect URL</Label>
                <Textarea
                  id="llm-final-redirect"
                  value={redirectURL}
                  autoComplete="off"
                  spellCheck={false}
                  rows={3}
                  placeholder={
                    attempt.callback_url
                      ? `${attempt.callback_url}?code=…&state=…`
                      : "Paste the complete final callback URL"
                  }
                  onChange={(event) => setRedirectURL(event.target.value)}
                />
                <p className="text-muted-foreground text-xs">
                  If the browser cannot reach the loopback callback, copy the complete final URL from its address bar.
                  Do not paste a token or authorization code by itself.
                </p>
                <Button
                  type="button"
                  size="sm"
                  className="w-fit"
                  disabled={!redirectURL.trim() || submitting}
                  onClick={() => void submitRedirect()}
                >
                  {submitting && <Loader2Icon className="animate-spin" />}
                  Submit final redirect
                </Button>
              </div>
            </CardContent>
          )}
        </Card>
      )}

      {flowError && (
        <Alert variant="destructive">
          <AlertCircleIcon />
          <AlertTitle>Account authorization needs attention</AlertTitle>
          <AlertDescription>{flowError}</AlertDescription>
        </Alert>
      )}
      {notice && (
        <Alert>
          <AlertCircleIcon />
          <AlertTitle>Disconnect result</AlertTitle>
          <AlertDescription>{notice}</AlertDescription>
        </Alert>
      )}
      {error && (
        <Alert variant="destructive">
          <AlertCircleIcon />
          <AlertTitle>Accounts unavailable</AlertTitle>
          <AlertDescription className="grid gap-2">
            <span>{error} Existing API-key and Ollama profiles remain available.</span>
            <Button type="button" size="sm" variant="outline" className="w-fit" onClick={() => void onRefresh()}>
              Retry account list
            </Button>
          </AlertDescription>
        </Alert>
      )}

      <div className="grid gap-2">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div>
            <h2 className="font-medium text-sm">Stored accounts</h2>
            <p className="text-muted-foreground text-xs">
              “Session stored” only describes local credential state. Model access, quota, balance, and billing are
              checked separately by the provider when used.
            </p>
          </div>
          <Button type="button" size="sm" variant="ghost" disabled={loading} onClick={() => void onRefresh()}>
            {loading ? <Loader2Icon className="animate-spin" /> : <RefreshCwIcon />}
            Refresh
          </Button>
        </div>

        {loading && credentials.length === 0 ? (
          <div className="flex items-center justify-center gap-2 rounded-lg border border-dashed p-8 text-muted-foreground text-sm">
            <Loader2Icon className="size-4 animate-spin" /> Loading accounts…
          </div>
        ) : credentials.length === 0 && !error ? (
          <div className="rounded-lg border border-dashed p-8 text-center text-muted-foreground text-sm">
            No stored accounts. Start an experimental browser authorization above.
          </div>
        ) : (
          <div className="grid gap-2">
            {credentials.map((credential) => (
              <div
                key={credential.id}
                className="grid gap-3 rounded-lg border p-3 sm:grid-cols-[minmax(0,1fr)_auto] sm:items-center"
              >
                <div className="min-w-0">
                  <div className="flex flex-wrap items-center gap-2">
                    {credential.status === "ready" && <CheckCircle2Icon className="size-4 text-emerald-500" />}
                    {credential.status !== "ready" && <AlertCircleIcon className="size-4 text-amber-500" />}
                    <span className="truncate font-medium text-sm">{credential.label}</span>
                    <Badge variant="outline" className={statusClass(credential.status)}>
                      {statusLabel(credential.status)}
                    </Badge>
                  </div>
                  <p className="mt-1 text-muted-foreground text-xs">
                    {providerName(credential.provider)} · Updated {formatTime(credential.updated_at)}
                  </p>
                </div>
                <div className="flex flex-wrap gap-2">
                  <Button
                    type="button"
                    size="sm"
                    variant="outline"
                    disabled={reauthenticatingID !== null || disconnectingID !== null}
                    onClick={() => void begin(credential.provider, credential.id)}
                  >
                    {reauthenticatingID === credential.id ? (
                      <Loader2Icon className="animate-spin" />
                    ) : (
                      <RefreshCwIcon />
                    )}
                    Reauthenticate
                  </Button>
                  <Button
                    type="button"
                    size="sm"
                    variant="outline"
                    disabled={disconnectingID !== null || credential.status === "revoked"}
                    onClick={() => void disconnect(credential)}
                  >
                    {disconnectingID === credential.id ? <Loader2Icon className="animate-spin" /> : <LogOutIcon />}
                    Disconnect
                  </Button>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
