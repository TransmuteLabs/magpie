export interface AccountKeepAliveDescriptor {
  leadMs?: number;
}

export function accountKeepAliveLead(policy?: AccountKeepAliveDescriptor | null): number;
