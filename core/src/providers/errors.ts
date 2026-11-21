export class ProviderNotReadyError extends Error {
  constructor(
    readonly kind: string,
    readonly provider: string,
    readonly phase: string,
  ) {
    super(`${kind} provider "${provider}" is not built yet (phase ${phase})`);
    this.name = "ProviderNotReadyError";
  }
}
