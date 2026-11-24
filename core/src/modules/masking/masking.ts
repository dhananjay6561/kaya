export type MaskKind =
  | "EMAIL"
  | "UPI"
  | "CARD"
  | "PHONE"
  | "AADHAAR"
  | "PAN"
  | "IFSC"
  | "PASSPORT"
  | "VEHICLE"
  | "ACCOUNT"
  | "ADDRESS"
  | "OTP";

export type MaskOptions = Readonly<{
  /** Text around this one (such as the question an answer replied to): its words count as context. */
  context?: string;
}>;

export type MaskSession = Readonly<{
  /** Replaces private details with placeholders such as [PHONE_1]; the same value gets the same one. */
  mask(text: string, options?: MaskOptions): string;
  /** Puts the original values back wherever a placeholder appears, even slightly reformatted. */
  restore(text: string): string;
  /** How many distinct values of each kind were masked. */
  counts(): Partial<Record<MaskKind, number>>;
}>;

type Span = { start: number; end: number; kind: MaskKind };

type Scan = Readonly<{ text: string; context: string; spans: Span[] }>;

type Rule = Readonly<{
  kind: MaskKind;
  pattern: RegExp;
  accept?: (match: string, start: number, scan: Scan) => boolean;
  /** Cut the match short at the next detail already found, instead of giving up on it. */
  clip?: boolean;
}>;

const kinds = "EMAIL|UPI|CARD|PHONE|AADHAAR|PAN|IFSC|PASSPORT|VEHICLE|ACCOUNT|ADDRESS|OTP";
const issuedShape = new RegExp(`\\[(${kinds})_(\\d+)\\]`, "g");
const bracketedShape = new RegExp(
  `\\[[ \\t]*(${kinds})[ \\t]*\\\\?[_ -][ \\t]*(\\d+)[ \\t]*\\]`,
  "gi",
);
const bareShape = new RegExp(`\\b(${kinds})\\\\?_(\\d+)\\b`, "gi");
const escapedShape = new RegExp(`［(${kinds})_(\\d+)］`, "g");

const digitsOf = (value: string) => value.replace(/\D/g, "");

const digitZeros = [
  0x660, 0x6f0, 0x966, 0x9e6, 0xa66, 0xae6, 0xb66, 0xbe6, 0xc66, 0xce6, 0xd66, 0xff10,
];

/** Same length as the input, with Indian, Arabic and full-width digits turned into ASCII ones. */
function asciiDigits(text: string): string {
  let result = "";
  for (let i = 0; i < text.length; i++) {
    const code = text.charCodeAt(i);
    const zero = digitZeros.find((z) => code >= z && code <= z + 9);
    result += zero === undefined ? text[i] : String(code - zero);
  }
  return result;
}

function luhn(digits: string): boolean {
  let sum = 0;
  for (let i = 0; i < digits.length; i++) {
    let d = Number(digits[digits.length - 1 - i]);
    if (i % 2 === 1) {
      d *= 2;
      if (d > 9) d -= 9;
    }
    sum += d;
  }
  return sum % 10 === 0;
}

const around = (scan: Scan, start: number, length: number, before: number, after: number) =>
  scan.text.slice(Math.max(0, start - before), start) +
  " " +
  scan.text.slice(start + length, start + length + after);

const strongOtpWords = /otp|passcode|pass code|one[\s-]?time|verification|login code|log in code/i;
const weakOtpBefore = /\b(?:code|pin|mpin)\b[\s:=-]*(?:is|hai|h|:)?[\s:=-]*$/i;
const weakOtpAfter =
  /^\s*(?:is|hai)?\s*(?:your|the|aapka|tumhara)?\s*(?:\w+\s+){0,2}(?:code|pin|otp)\b/i;
const moneyBefore = /(?:rs\.?|inr|₹|\$|usd|eur|€)\s*$/i;
const accountWords =
  /a\/c|acc(?:oun)?t|acct|khata|ifsc|neft|imps|rtgs|bank|hdfc|icici|sbi|axis|kotak|pnb|canara|idfc|indusind|yes\s?bank|bob|federal/i;
const addressWords =
  /\b(?:road|rd|street|st|marg|nagar|sector|sec|colony|lane|gali|block|apartments?|apts?|society|chowk|tower|floor|layout|enclave|vihar|bagh|ganj|puram|village|district|mohalla|extension|ext|cross|main|phase)\b/i;

const rules: readonly Rule[] = [
  { kind: "EMAIL", pattern: /[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}/g },
  {
    kind: "UPI",
    pattern: /[A-Za-z0-9._-]{2,}@[A-Za-z]{2,}(?![\w-]*\.[A-Za-z]{2,})(?![A-Za-z0-9])/g,
  },
  {
    kind: "CARD",
    pattern: /(?<![\d.])\d(?:[ .-]?\d){12,18}(?![\d])/g,
    accept: (match) => luhn(digitsOf(match)),
  },
  {
    kind: "PHONE",
    pattern: /(?:(?:\+|00)[ ]?)?\(?\d(?:[ ().-]{0,2}\d){7,14}/g,
    accept: (match, start, scan) => {
      if (
        /\d/.test(scan.text[start - 1] ?? "") ||
        /\d/.test(scan.text[start + match.length] ?? "")
      ) {
        return false;
      }
      const digits = digitsOf(match);
      if (/^\s*(?:\+|00)/.test(match)) return digits.length >= 8 && digits.length <= 15;
      if (/^(?:91|0)?[6-9]\d{9}$/.test(digits)) return true;
      return /^0\d{9,10}$/.test(digits) && /^\(?0\d{2,4}\)?[ -]/.test(match);
    },
  },
  {
    kind: "AADHAAR",
    pattern: /(?<![\d+][ -]?)[2-9]\d{3}[ -]?\d{4}[ -]?\d{4}(?![ -]?\d)/g,
  },
  { kind: "PAN", pattern: /\b[A-Za-z]{5}\d{4}[A-Za-z]\b/g, accept: (m) => /[A-Za-z]$/.test(m) },
  {
    kind: "IFSC",
    pattern: /\b[A-Za-z]{4}0[A-Za-z0-9]{6}\b/g,
    accept: (m) => /\d/.test(m.slice(5)),
  },
  {
    kind: "VEHICLE",
    pattern: /\b[A-Z]{2}[ -]?\d{1,2}[ -]?[A-Z]{1,2}(?:[ -]?[A-Z]{1,3})?[ -]?\d{4}\b/g,
  },
  {
    kind: "PASSPORT",
    pattern: /\b[A-Z][1-9]\d{6}\b/g,
  },
  {
    kind: "ACCOUNT",
    pattern: /(?<!\d[ -]?)\d(?:[ ]?\d){8,17}(?![ -]?\d)/g,
    accept: (match, start, scan) =>
      accountWords.test(around(scan, start, match.length, 40, 25)) ||
      accountWords.test(scan.context),
  },
  {
    kind: "ADDRESS",
    pattern:
      /\b(?:flat|plot|house|h\.?[ \t]?no|door|shop)\.?[ \t]{0,3}(?:no\.?|number|#)?[ \t]{0,3}[:-]?[ \t]{0,3}[A-Za-z]?-?\d+[A-Za-z]?(?:[/-]\d+[A-Za-z]?)?/gi,
    accept: (match) => /^(?:flat|plot)\b/i.test(match) || /\b(?:no|number)\b|#/i.test(match),
  },
  {
    kind: "ADDRESS",
    pattern:
      /(?:pin[ \t]?code|pincode|postal code|zip)[ \t]{0,3}[:-]?[ \t]{0,3}[1-9]\d{2}[ \t]?\d{3}\b/gi,
  },
  {
    kind: "ADDRESS",
    pattern:
      /(?<=^|[,:;\n][ \t]{0,3})(?:[A-Za-z]{1,2}-?)?\d{1,4}[A-Za-z]?(?:[/-]\d{1,4})?,?[ \t][^\n]{2,120}/gm,
    accept: (match) => addressWords.test(match),
    clip: true,
  },
  {
    kind: "ADDRESS",
    pattern: /[^\n,]{0,60}\b[1-9]\d{2}[ \t]?\d{3}\b/g,
    accept: (match) => addressWords.test(match),
    clip: true,
  },
  {
    kind: "OTP",
    pattern: /(?<![\d,/_.]|\d\.|\d:)\d(?:[ -]?\d){3,7}(?![\d,:/%]|\.\d)/g,
    accept: (match, start, scan) => {
      const digits = digitsOf(match);
      if (/^(?:19|20)\d{2}$/.test(digits)) return false;
      if (moneyBefore.test(scan.text.slice(Math.max(0, start - 6), start))) return false;
      if (strongOtpWords.test(around(scan, start, match.length, 40, 40))) return true;
      if (strongOtpWords.test(scan.context)) return true;
      const before = scan.text.slice(Math.max(0, start - 16), start);
      const after = scan.text.slice(start + match.length, start + match.length + 40);
      return weakOtpBefore.test(before) || weakOtpAfter.test(after);
    },
  },
];

function find(rule: Rule, scan: Scan): void {
  const pattern = new RegExp(rule.pattern.source, rule.pattern.flags);
  for (let match = pattern.exec(scan.text); match; match = pattern.exec(scan.text)) {
    let start = match.index;
    let end = start + match[0].length;
    while (start < end && /\s/.test(scan.text[start] ?? "")) start++;
    while (end > start && /[\s,.;:]/.test(scan.text[end - 1] ?? "")) end--;
    const inside = scan.spans.find((span) => start < span.end && span.start < end);
    if (inside && rule.clip && inside.start > start) {
      end = inside.start;
      while (end > start && /[\s,.;:]/.test(scan.text[end - 1] ?? "")) end--;
    }
    const clash = scan.spans.some((span) => start < span.end && span.start < end);
    const value = scan.text.slice(start, end);
    if (end <= start || clash || (rule.accept && !rule.accept(value, start, scan))) {
      pattern.lastIndex = match.index + 1;
      continue;
    }
    scan.spans.push({ start, end, kind: rule.kind });
  }
}

function normalized(kind: MaskKind, value: string): string {
  switch (kind) {
    case "EMAIL":
    case "UPI":
      return value.toLowerCase();
    case "ADDRESS":
      return value.toLowerCase().replace(/\s+/g, " ");
    case "PAN":
    case "IFSC":
    case "VEHICLE":
    case "PASSPORT":
      return value.toUpperCase().replace(/[\s-]/g, "");
    case "PHONE":
      return digitsOf(value).replace(/^(?:0091|91|0)(?=[6-9]\d{9}$)/, "");
    default:
      return digitsOf(value);
  }
}

export function createMaskSession(): MaskSession {
  const byValue = new Map<string, string>();
  const byPlaceholder = new Map<string, string>();
  const seen: Partial<Record<MaskKind, number>> = {};

  const placeholderFor = (kind: MaskKind, value: string, shadow: string) => {
    const key = `${kind}:${normalized(kind, shadow)}`;
    const known = byValue.get(key);
    if (known) return known;
    const n = (seen[kind] ?? 0) + 1;
    seen[kind] = n;
    const made = `[${kind}_${n}]`;
    byValue.set(key, made);
    byPlaceholder.set(made, value);
    return made;
  };

  return Object.freeze({
    mask(text: string, options: MaskOptions = {}) {
      const escaped = text.replace(
        issuedShape,
        (_literal, kind: string, n: string) => `［${kind}_${n}］`,
      );
      const scan: Scan = {
        text: asciiDigits(escaped),
        context: asciiDigits(options.context ?? ""),
        spans: [],
      };
      for (const rule of rules) find(rule, scan);
      let result = "";
      let at = 0;
      for (const span of scan.spans.sort((a, b) => a.start - b.start)) {
        result += escaped.slice(at, span.start);
        result += placeholderFor(
          span.kind,
          escaped.slice(span.start, span.end),
          scan.text.slice(span.start, span.end),
        );
        at = span.end;
      }
      return result + escaped.slice(at);
    },
    restore(text: string) {
      return text
        .split(escapedShape)
        .map((part, i) => {
          if (i % 3 === 1) return `[${part}_`;
          if (i % 3 === 2) return `${part}]`;
          const issued = (found: string, kind: string, n: string) =>
            byPlaceholder.get(`[${kind.toUpperCase()}_${n}]`) ?? found;
          return part.replace(bracketedShape, issued).replace(bareShape, issued);
        })
        .join("");
    },
    counts() {
      return { ...seen };
    },
  });
}
