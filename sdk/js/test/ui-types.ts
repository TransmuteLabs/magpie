import {
  ui, type UIAction, type UIConfigSnapshot, type UIContext, type UIEntity, type UITheme,
} from "../ui.js";

const ready: Promise<UITheme> = ui.ready;
const theme: UITheme | null = ui.theme;
const context: UIContext = ui.context;
const visible: boolean = ui.visible;
const entity: UIEntity = { id: "fixture", name: "Fixture" };
const action: UIAction = { id: "fixture.open", provider: entity };
const png = new Blob(["fixture"], { type: "image/png" });
ui.onTheme(value => { const language: string = value.lang; void language; });
ui.onVisibility(value => { const shown: boolean = value; void shown; });
ui.onAction(async value => { const id: string = value.id; void id; });
const read: Promise<{ items: string[] }> = ui.read("providers:list", { fixture: true });
const copied: Promise<{ ok: true }> = ui.image.copy(png);
const saved: Promise<{ path: string }> = ui.image.save(png, "fixture");
const savedDefault: Promise<{ path: string }> = ui.image.save(png);
const configured: Promise<UIConfigSnapshot> = ui.config.cas("0", { fixture: true, nested: { rows: [] } });
type FalseOnly<Value extends false> = Value;
type NullConfigurationRejected = FalseOnly<null extends Parameters<typeof ui.config.cas>[1] ? true : false>;
type ArrayConfigurationRejected = FalseOnly<[] extends Parameters<typeof ui.config.cas>[1] ? true : false>;
void ready; void theme; void context; void visible; void action; void read;
void copied; void saved; void savedDefault; void configured;
