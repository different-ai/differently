/**
 * Error codes. The first group comes from the server; branch on `code`,
 * never on `message`. `timeout` and `connection_error` are raised by the SDK
 * itself and carry `status: 0`.
 */
export type ErrorCode =
  | "invalid_request"
  | "unauthenticated"
  | "permission_denied"
  | "not_found"
  | "conflict"
  | "quota_exceeded"
  | "no_eligible_pool"
  | "unsupported_capability"
  | "unsupported_operation"
  | "provider_unavailable"
  | "invalid_state"
  | "rate_limited"
  | "suspended"
  | "internal"
  | "timeout"
  | "connection_error"
  | (string & {});

export interface DifferentlyErrorInit {
  code: ErrorCode;
  message: string;
  /** HTTP status, or 0 when the error did not come from an HTTP response. */
  status?: number;
  details?: Record<string, string>;
  requestId?: string;
  cause?: unknown;
}

/** Every failure the SDK reports is a DifferentlyError. */
export class DifferentlyError extends Error {
  readonly code: ErrorCode;
  readonly status: number;
  readonly details: Record<string, string>;
  readonly requestId: string | undefined;

  constructor(init: DifferentlyErrorInit) {
    super(init.message, init.cause === undefined ? undefined : { cause: init.cause });
    this.name = "DifferentlyError";
    this.code = init.code;
    this.status = init.status ?? 0;
    this.details = init.details ?? {};
    this.requestId = init.requestId;
  }
}

const statusCodes: Record<number, ErrorCode> = {
  400: "invalid_request",
  401: "unauthenticated",
  403: "permission_denied",
  404: "not_found",
  409: "conflict",
  422: "no_eligible_pool",
  429: "rate_limited",
  501: "unsupported_operation",
  503: "provider_unavailable",
};

/** Builds an error from a non-2xx response, tolerating non-JSON bodies from proxies. */
export function errorFromResponse(status: number, body: string, headerRequestId: string | null): DifferentlyError {
  let parsed: { code?: unknown; message?: unknown; details?: unknown; requestId?: unknown } | undefined;
  try {
    const envelope = JSON.parse(body) as { error?: typeof parsed };
    if (envelope && typeof envelope.error === "object" && envelope.error !== null) parsed = envelope.error;
  } catch {
    // Not a Differently error envelope.
  }
  const requestId =
    typeof parsed?.requestId === "string" && parsed.requestId !== "" ? parsed.requestId : headerRequestId ?? undefined;
  return new DifferentlyError({
    code: typeof parsed?.code === "string" ? parsed.code : statusCodes[status] ?? "internal",
    message:
      typeof parsed?.message === "string" ? parsed.message : `HTTP ${status}${body ? `: ${body.slice(0, 200)}` : ""}`,
    status,
    details: isStringRecord(parsed?.details) ? parsed.details : {},
    requestId,
  });
}

function isStringRecord(v: unknown): v is Record<string, string> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}
