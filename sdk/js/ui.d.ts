/** JSON carried by the browser page bridge; uint64 versions are strings. */
export type UIValue = null | boolean | number | string | UIValue[] | { [key: string]: UIValue };
export interface UIConfigValues { [key: string]: UIValue }
export interface UIConfigSnapshot { version: string; values: UIConfigValues }
export interface AccountSelection { id: string; expectedGeneration: string }
export interface AccountSummary { id: string; generation: string; expiresAt?: number; name?: string }
export interface WindowCallOptions { account?: AccountSelection }
export interface WindowAuthStartOptions { method: string; input?: UIValue; account?: AccountSelection }
export interface WindowAuthFlowView {
  flow: string; state: "waiting" | "pending" | "complete" | "canceled" | "expired" | "uncertain";
  url?: string; userCode?: string; retryAfterMs?: number; account?: AccountSummary;
}
/** Selectable HTTPS approval address; no navigation or clipboard authority. */
export interface AuthLinkPresentation {
  update(flow: WindowAuthFlowView | null): void;
  dispose(): void;
}
export function createAuthLinkPresentation(elements: {
  container: HTMLElement;
  input: HTMLInputElement;
  select: HTMLButtonElement;
}): AuthLinkPresentation;
export interface UITheme {
  readonly dark: boolean;
  readonly lang: string;
  readonly tokens: Readonly<Record<string, string>>;
}
export interface UIEntity { readonly id: string; readonly name: string }
export interface UIContext {
  readonly provider?: UIEntity;
  readonly agent?: UIEntity;
}
export interface UIAction {
  readonly id: string;
  readonly provider?: UIEntity;
  readonly agent?: UIEntity;
}
export interface HistorySelection { source: string; viewRef: string }
export interface HistoryRoot {
  slot: string; rootRef?: string; consentRef?: string; available: boolean; consented: boolean;
}
export interface HistoryInspection {
  module: string; source: string; agent: string; viewRef: string; readAllowed: boolean;
  ownOff: boolean; roots: HistoryRoot[]; replaces: string[];
}
export interface HistoryAccess {
  state: "ready" | "released" | "canceled" | "changed" | "denied" | "unavailable";
  inspection: HistoryInspection | null; error?: string;
}
export interface HistoryRootSelection extends HistorySelection { rootRef: string }
export interface HistorySessionSelection extends HistorySelection { sessionRef: string }
export interface HistoryMediaSelection extends HistorySessionSelection { readRef: string; mediaRef: string }
export interface HistorySummary { sessionRef: string; title: string; start?: string; last?: string }
export interface HistoryPage { entries: HistorySummary[]; nextCursor?: string }
export interface HistoryMedia { mediaRef: string; name: string; mime: string; size: number }
export interface HistoryPart { role: string; kind: string; name?: string; text: string; model?: string; cut?: number }
export interface HistoryTranscript {
  sessionRef: string; readRef: string; parts: HistoryPart[]; cut: boolean; media: HistoryMedia[];
}
export interface SessionHistory {
  readonly ready: Promise<boolean>;
  inspect(source: string): Promise<HistoryInspection>;
  requestAccess(selection: HistoryRootSelection): Promise<HistoryAccess>;
  release(selection: HistorySelection & { consentRef: string }): Promise<HistoryAccess>;
  list(selection: HistoryRootSelection & { cursor?: string; limit?: number }): Promise<HistoryPage>;
  read(selection: HistorySessionSelection): Promise<HistoryTranscript>;
  media(selection: HistoryMediaSelection): Promise<{ mime: string; data: ArrayBuffer }>;
}
export interface ModuleUI {
  readonly ready: Promise<UITheme>;
  readonly service: string;
  readonly theme: UITheme | null;
  readonly context: UIContext;
  readonly visible: boolean;
  readonly history: SessionHistory;
  onTheme(listener: (theme: UITheme) => void): void;
  onVisibility(listener: (visible: boolean) => void): void;
  onAction(listener: (action: UIAction) => void | Promise<void>): void;
  read<T = UIValue>(name: string, params?: UIValue): Promise<T>;
  readonly image: {
    copy(png: Blob): Promise<{ ok: true }>;
    save(png: Blob, name?: string): Promise<{ path: string }>;
  };
  readonly backend: {
    /** Unary. Frame disposal/update cancellation remains owned by the bridge. */
    call<T = UIValue>(binding: string, input?: UIValue, options?: WindowCallOptions): Promise<T>;
    accounts(binding: string): Promise<AccountSummary[]>;
  };
  readonly auth: {
    start(binding: string, options: WindowAuthStartOptions): Promise<WindowAuthFlowView>;
    finish(binding: string, flow: string): Promise<WindowAuthFlowView>;
    cancel(binding: string, flow: string): Promise<WindowAuthFlowView>;
  };
  readonly config: {
    get(): Promise<UIConfigSnapshot>;
    cas(expected: string, values: UIConfigValues): Promise<UIConfigSnapshot>;
  };
}
export const ui: ModuleUI;

export interface AccountWorkflowSnapshot {
  readonly accounts: readonly Readonly<AccountSummary>[];
  readonly selection: Readonly<AccountSelection> | null;
  readonly stale: boolean;
  readonly flow: Readonly<WindowAuthFlowView> | null;
  readonly busy: boolean;
}
export interface AccountWorkflow {
  readonly snapshot: AccountWorkflowSnapshot;
  refresh(): Promise<readonly Readonly<AccountSummary>[]>;
  select(id: string | null): Readonly<AccountSelection> | null;
  call<T = UIValue>(input?: UIValue): Promise<T>;
  start(options: { method: string; input?: UIValue }, reauthorize?: boolean): Promise<WindowAuthFlowView>;
  finish(): Promise<WindowAuthFlowView>;
  cancel(): Promise<WindowAuthFlowView>;
}
export function createAccountWorkflow(binding: string, authBinding?: string): AccountWorkflow;
