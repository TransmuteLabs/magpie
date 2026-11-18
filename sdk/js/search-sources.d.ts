export interface SearchSourceDescriptor { id: string; name: string; action: string; provider?: string }
export interface SearchRequest { source: string; query: string; count: number; proxy?: string }
export interface SearchPage { title: string; url: string; text: string; age?: string }
export interface SearchResult { pages: SearchPage[]; status?: number; error?: string; retryAfter?: string }
export function validateSearchRequest(request: unknown): SearchRequest;
export function validateSearchResult(result: unknown, count: number): SearchResult;
