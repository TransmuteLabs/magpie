/** JSON carried by the browser page bridge; uint64 versions are strings. */
export type UIValue = null | boolean | number | string | UIValue[] | { [key: string]: UIValue };
export interface UIConfigValues { [key: string]: UIValue }
export interface UIConfigSnapshot { version: string; values: UIConfigValues }
export interface AccountSelection { id: string; expectedGeneration: string }
export interface AccountSummary { id: string; generation: string; expiresAt?: number }
export interface WindowCallOptions { account?: AccountSelection }
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
export interface ModuleUI {
  readonly ready: Promise<UITheme>;
  readonly service: string;
  readonly theme: UITheme | null;
  readonly context: UIContext;
  readonly visible: boolean;
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
  readonly config: {
    get(): Promise<UIConfigSnapshot>;
    cas(expected: string, values: UIConfigValues): Promise<UIConfigSnapshot>;
  };
}
export const ui: ModuleUI;
