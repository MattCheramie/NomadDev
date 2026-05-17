// Exponential backoff with a 30s ceiling.
// 1s → 2s → 4s → 8s → 16s → 30s → 30s → ... — matches the plan.
export class Backoff {
  private attempts = 0;
  constructor(private readonly baseMs = 1000, private readonly capMs = 30000) {}

  nextMs(): number {
    const delay = Math.min(this.baseMs * Math.pow(2, this.attempts), this.capMs);
    this.attempts++;
    return delay;
  }

  reset(): void {
    this.attempts = 0;
  }
}
