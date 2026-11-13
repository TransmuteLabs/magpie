/** JSON carried by the browser page bridge; uint64 versions are strings. */
export type UIValue = null | boolean | number | string | UIValue[] | { [key: string]: UIValue };
export interface UIConfigSnapshot { version: string; values: UIValue }
export interface ModuleUI {
  readonly ready: Promise<unknown>;
  readonly service: string;
  readonly backend: {
    /** Unary. Frame disposal/update cancellation remains owned by the bridge. */
    call<T = UIValue>(binding: string, input?: UIValue): Promise<T>;
  };
  readonly config: {
    get(): Promise<UIConfigSnapshot>;
    cas(expected: string, values: UIValue): Promise<UIConfigSnapshot>;
  };
}
export const ui: ModuleUI;
