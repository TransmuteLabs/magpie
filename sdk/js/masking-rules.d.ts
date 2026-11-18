export interface MaskingRule {
  kind: string;
  prefix?: string;
  regex?: string;
}
export interface MaskingRuleSet {
  id: string;
  rules: readonly MaskingRule[];
}
