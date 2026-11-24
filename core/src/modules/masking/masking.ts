export type MaskKind =
  | "EMAIL"
  | "UPI"
  | "CARD"
  | "AADHAAR"
  | "PAN"
  | "IFSC"
  | "PHONE"
  | "ACCOUNT"
  | "OTP"
  | "ADDRESS";

export type MaskSession = Readonly<{
  /** Replaces private details with placeholders such as [PHONE_1]; the same value gets the same one. */
  mask(text: string): string;
  /** Puts the original values back wherever a placeholder appears. */
  restore(text: string): string;
  /** How many distinct values of each kind were masked. */
  counts(): Partial<Record<MaskKind, number>>;
}>;

type Rule = Readonly<{
  kind: MaskKind;
  pattern: RegExp;
  accept?: (match: string, text: string) => boolean;
}>;

const digitsOf = (value: string) => value.replace(/\D/g, "");

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

const otpWords = /\b(otp|one[\s-]?time|verification|verify|passcode|pass\s?code|pin|code)\b/i;
const accountWords = /\b(a\/c|acc(?:oun)?t|acct|account\s*(?:no|number)|khata)\b[^\d\n]{0,20}$/i;
const addressWords =
  /\b(road|rd|street|st|nagar|sector|colony|marg|lane|block|apartments?|apt|society|chowk|phase|tower|floor|layout|enclave|vihar|bagh|ganj|puram|pur|village|district|near)\b/i;

const rules: readonly Rule[] = [
  { kind: "EMAIL", pattern: /[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/g },
  { kind: "UPI", pattern: /\b[A-Za-z0-9._-]{2,}@[A-Za-z]{2,}\b(?!\.)/g },
  {
    kind: "CARD",
    pattern: /(?<!\d[ -]?)\d(?:[ -]?\d){12,18}(?![ -]?\d)/g,
    accept: (match) => luhn(digitsOf(match)),
  },
  {
    kind: "PHONE",
    pattern:
      /(?<![\w+])(?:(?:\+|00)\d{1,3}[\s-]?)?(?:\(?\d{2,5}\)?[\s-]?)?\d{3,5}[\s-]?\d{4,5}(?!\d)/g,
    accept: (match) => {
      const digits = digitsOf(match);
      if (/^(?:\+|00)/.test(match.trim())) return digits.length >= 8 && digits.length <= 15;
      const local = digits.replace(/^(?:91|0)(?=\d{10}$)/, "");
      return /^[6-9]\d{9}$/.test(local);
    },
  },
  { kind: "AADHAAR", pattern: /(?<![\d+][ -]?|\d)[2-9]\d{3}[ -]?\d{4}[ -]?\d{4}(?![ -]?\d)/g },
  { kind: "PAN", pattern: /\b[A-Z]{5}\d{4}[A-Z]\b/g },
  { kind: "IFSC", pattern: /\b[A-Z]{4}0[A-Z0-9]{6}\b/g },
  {
    kind: "ACCOUNT",
    pattern: /(?<!\d)\d{9,18}(?!\d)/g,
    accept: (match, text) => {
      const at = text.indexOf(match);
      return at >= 0 && accountWords.test(text.slice(Math.max(0, at - 40), at));
    },
  },
  {
    kind: "ADDRESS",
    pattern:
      /\b(?:flat|house|h\.?\s?no|plot|door|shop)\.?\s*(?:no\.?|number|#)?\s*[:-]?\s*[A-Za-z]?-?\d+[A-Za-z]?(?:[/-]\d+[A-Za-z]?)?/gi,
  },
  {
    kind: "ADDRESS",
    pattern: /^.*\b[1-9]\d{2}\s?\d{3}\b.*$/gm,
    accept: (match) => addressWords.test(match) || /\bpin\s*(?:code)?\b/i.test(match),
  },
  {
    kind: "OTP",
    pattern: /(?<![\d,:/₹$]|\d\.)\d{4,8}(?![\d,:/%]|\.\d)/g,
    accept: (match, text) => otpWords.test(text) && !/^(?:19|20)\d{2}$/.test(match),
  },
];

const placeholder = /\[(EMAIL|UPI|CARD|AADHAAR|PAN|IFSC|PHONE|ACCOUNT|OTP|ADDRESS)_(\d+)\]/g;

function normalized(kind: MaskKind, value: string): string {
  if (kind === "EMAIL" || kind === "UPI") return value.toLowerCase();
  if (kind === "ADDRESS") return value.toLowerCase().replace(/\s+/g, " ");
  const digits = digitsOf(value);
  return kind === "PHONE" ? digits.replace(/^(?:0091|91|0)(?=[6-9]\d{9}$)/, "") : digits || value;
}

export function createMaskSession(): MaskSession {
  const byValue = new Map<string, string>();
  const byPlaceholder = new Map<string, string>();
  const seen: Partial<Record<MaskKind, number>> = {};

  const placeholderFor = (kind: MaskKind, value: string) => {
    const key = `${kind}:${normalized(kind, value)}`;
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
    mask(text: string) {
      let result = text;
      for (const rule of rules) {
        const source = result;
        result = result.replace(rule.pattern, (match) => {
          if (/^\[[A-Z]+_\d+\]$/.test(match.trim())) return match;
          if (rule.accept && !rule.accept(match, source)) return match;
          return placeholderFor(rule.kind, match.trim());
        });
      }
      return result;
    },
    restore(text: string) {
      let result = text;
      for (let i = 0; i < 3; i++) {
        const next = result.replace(placeholder, (made) => byPlaceholder.get(made) ?? made);
        if (next === result) break;
        result = next;
      }
      return result;
    },
    counts() {
      return { ...seen };
    },
  });
}
