export type CallRequest = Readonly<{ to: string; brief: string }>;

export type CallStatus =
  | "queued"
  | "ringing"
  | "in-progress"
  | "completed"
  | "no-answer"
  | "failed";

export type Call = Readonly<{
  id: string;
  to: string;
  status: CallStatus;
  startedAt?: Date;
  endedAt?: Date;
  transcript?: string;
}>;

export interface TelephonyProvider {
  readonly name: string;
  placeCall(request: CallRequest, options?: Readonly<{ signal?: AbortSignal }>): Promise<Call>;
  getCall(id: string): Promise<Call | undefined>;
}

export const e164 = /^\+[1-9]\d{7,14}$/;
