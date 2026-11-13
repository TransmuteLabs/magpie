export class SDKError extends Error {
  constructor(code, message, outcome) {
    if (typeof code !== "string" || !/^[a-zA-Z0-9_.-]{1,64}$/.test(code)
      || typeof message !== "string" || !message.trim() || new TextEncoder().encode(message).byteLength > 4096
      || /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/.test(message)
      || outcome !== undefined && (typeof outcome !== "string" || !/^[a-zA-Z0-9_.-]{1,64}$/.test(outcome))) {
      throw new TypeError("Invalid public SDK error");
    }
    super(message);
    this.name = "SDKError";
    this.code = code;
    if (outcome !== undefined) this.outcome = outcome;
  }
}
