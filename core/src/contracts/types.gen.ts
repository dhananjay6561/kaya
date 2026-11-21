// Generated from contracts/schemas by core/src/contracts/generate-types.ts. Run `make contracts`; do not edit.

/**
 * Any payload in the gateway ↔ core contract
 */
export type ContractPayload = GatewayEvent | SendRequest;
/**
 * Something that happened in the gateway, sent to core. Core handles each id at most once.
 */
export type GatewayEvent = {
  type: "connection.state" | "message.stored" | "history.synced" | "send.result";
} & (ConnectionStateEvent | MessageStoredEvent | HistorySyncedEvent | SendResultEvent);
/**
 * RFC 3339 time in UTC
 */
export type Timestamp = string;

export interface ConnectionStateEvent {
  /**
   * Unique per event; the key for at-most-once handling
   */
  id: string;
  version: 1;
  type: "connection.state";
  occurred_at: Timestamp;
  data: ConnectionState;
}
export interface ConnectionState {
  state: "connecting" | "pairing" | "connected" | "disconnected" | "logged_out";
  /**
   * DJ's own JID; present once paired
   */
  own_jid?: string;
  /**
   * Why the state changed, for disconnected and logged_out
   */
  reason?: string;
}
export interface MessageStoredEvent {
  /**
   * Unique per event; the key for at-most-once handling
   */
  id: string;
  version: 1;
  type: "message.stored";
  occurred_at: Timestamp;
  data: MessageStored;
}
/**
 * A message row was written. The body stays in the database; core reads it by message_id.
 */
export interface MessageStored {
  /**
   * messages.id in the database
   */
  message_id: number;
  /**
   * WhatsApp JID without a device part, e.g. 919812345678@s.whatsapp.net, 120363000000000000@g.us or 100000000000001@lid
   */
  chat_jid: string;
  /**
   * WhatsApp JID without a device part, e.g. 919812345678@s.whatsapp.net, 120363000000000000@g.us or 100000000000001@lid
   */
  sender_jid: string;
  /**
   * WhatsApp message id
   */
  wa_id: string;
  from_me: boolean;
  kind:
    | "text"
    | "image"
    | "video"
    | "audio"
    | "voice"
    | "document"
    | "sticker"
    | "location"
    | "contact"
    | "poll"
    | "other";
  source: "live" | "history";
  change: "new" | "edited" | "deleted";
  sent_at: Timestamp;
}
export interface HistorySyncedEvent {
  /**
   * Unique per event; the key for at-most-once handling
   */
  id: string;
  version: 1;
  type: "history.synced";
  occurred_at: Timestamp;
  data: HistorySynced;
}
/**
 * One batch of the history sync was stored
 */
export interface HistorySynced {
  chats: number;
  messages: number;
  /**
   * Percent of the sync WhatsApp reports as done
   */
  progress?: number;
  complete: boolean;
}
export interface SendResultEvent {
  /**
   * Unique per event; the key for at-most-once handling
   */
  id: string;
  version: 1;
  type: "send.result";
  occurred_at: Timestamp;
  data: SendResult;
}
/**
 * What happened to a send request
 */
export interface SendResult {
  request_id: string;
  status: "sent" | "failed";
  /**
   * WhatsApp message id
   */
  wa_id?: string;
  error?: ErrorDetail;
}
export interface ErrorDetail {
  code: string;
  message: string;
}
/**
 * A message core asks the gateway to send. Core only creates one after DJ confirmed it (or for its own chat with DJ). The gateway sends each id at most once.
 */
export interface SendRequest {
  /**
   * Unique per request; the key for at-most-once sending
   */
  id: string;
  version: 1;
  created_at: Timestamp;
  /**
   * WhatsApp JID without a device part, e.g. 919812345678@s.whatsapp.net, 120363000000000000@g.us or 100000000000001@lid
   */
  to: string;
  content: TextContent;
  reply_to?: ReplyTo;
}
export interface TextContent {
  type: "text";
  text: string;
}
/**
 * The message this one quotes
 */
export interface ReplyTo {
  /**
   * WhatsApp message id
   */
  wa_id: string;
  /**
   * WhatsApp JID without a device part, e.g. 919812345678@s.whatsapp.net, 120363000000000000@g.us or 100000000000001@lid
   */
  sender_jid: string;
}
