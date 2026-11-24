import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { createMaskSession } from "./masking.js";

const masked = (text: string) => createMaskSession().mask(text);

describe("masking", () => {
  it("hides Indian and international phone numbers in the ways people write them", () => {
    for (const phone of [
      "9876543210",
      "+91 98765 43210",
      "+919876543210",
      "098765-43210",
      "91-98765-43210",
      "+1 415 555 2671",
      "+44 20 7946 0958",
    ]) {
      assert.equal(masked(`call me on ${phone} tomorrow`), "call me on [PHONE_1] tomorrow", phone);
    }
  });

  it("hides emails and UPI ids", () => {
    assert.equal(masked("mail rahul.k@gmail.com"), "mail [EMAIL_1]");
    assert.equal(masked("pay on rahul99@okaxis please"), "pay on [UPI_1] please");
  });

  it("hides OTPs and codes, but only where the message is about one", () => {
    assert.equal(
      masked("Your OTP is 482913. Do not share it."),
      "Your OTP is [OTP_1]. Do not share it.",
    );
    assert.equal(masked("482913 is your verification code"), "[OTP_1] is your verification code");
    assert.equal(masked("X pe dm passcode is 6561"), "X pe dm passcode is [OTP_1]");
    assert.equal(masked("meet at 4521 Main St"), "meet at 4521 Main St", "no code word, no OTP");
  });

  it("hides card numbers that pass the Luhn check, and only those", () => {
    assert.equal(masked("card 4111 1111 1111 1111 exp 12/29"), "card [CARD_1] exp 12/29");
    assert.equal(masked("card 4111-1111-1111-1111"), "card [CARD_1]");
    assert.equal(masked("ref 1234 5678 9012 3456"), "ref 1234 5678 9012 3456");
  });

  it("hides bank details and government ids", () => {
    assert.equal(
      masked("A/c no: 50100234567891, IFSC HDFC0001234"),
      "A/c no: [ACCOUNT_1], IFSC [IFSC_1]",
    );
    assert.equal(masked("account number 123456789012"), "account number [ACCOUNT_1]");
    assert.equal(masked("aadhaar 2345 6789 0123"), "aadhaar [AADHAAR_1]");
    assert.equal(masked("PAN ABCDE1234F"), "PAN [PAN_1]");
  });

  it("hides addresses: house numbers and lines with a PIN code", () => {
    assert.equal(masked("Flat No. 302, Sunshine Apartments"), "[ADDRESS_1], Sunshine Apartments");
    assert.equal(masked("H.No 14/2"), "[ADDRESS_1]");
    assert.equal(
      masked("send it here:\n12 MG Road, Sector 15, Gurugram 122001\nthanks"),
      "send it here:\n[ADDRESS_1]\nthanks",
    );
    assert.equal(masked("pincode 110001"), "[ADDRESS_1]");
  });

  it("leaves everyday numbers alone", () => {
    for (const text of [
      "it costs ₹4,500 for 2 people",
      "see you at 10:30 on 25/11/2026",
      "the meeting is in 2026",
      "order #4839201 shipped",
      "flight 6E 2341 at gate 12",
      "my score was 98.5%",
      "call at 5 pm, 3 of us",
    ]) {
      assert.equal(masked(text), text, text);
    }
  });

  it("gives the same value the same placeholder across texts, and puts them all back", () => {
    const session = createMaskSession();
    const one = session.mask("Rahul: 9876543210, rahul@x.com");
    const two = session.mask("again +91 98765 43210 and 9123456780");
    assert.equal(one, "Rahul: [PHONE_1], [EMAIL_1]");
    assert.equal(two, "again [PHONE_1] and [PHONE_2]");
    assert.equal(
      session.restore("Call [PHONE_1] or [PHONE_2], mail [EMAIL_1]"),
      "Call 9876543210 or 9123456780, mail rahul@x.com",
    );
    assert.equal(session.restore("unknown [PHONE_9] stays"), "unknown [PHONE_9] stays");
    assert.deepEqual(session.counts(), { PHONE: 2, EMAIL: 1 });
  });

  it("restores an address line that had another detail inside it", () => {
    const session = createMaskSession();
    const text = "12 MG Road, Gurugram 122001, call 9876543210";
    const hidden = session.mask(text);
    assert.doesNotMatch(hidden, /9876543210|122001|MG Road/);
    assert.equal(session.restore(hidden), text);
  });

  it("never touches its own placeholders", () => {
    const session = createMaskSession();
    const once = session.mask("OTP 482913 for account 50100234567891");
    assert.equal(session.mask(once), once);
  });

  it("gives back exactly the original text after a round trip", () => {
    for (const text of [
      "https://x.com/someone/status/1859340012345678901 and https://wa.me/919876543210",
      "OTP 482913, card 4111 1111 1111 1111, flat 302, Sector 15 Gurugram 122001",
      "nothing private here at all",
    ]) {
      const session = createMaskSession();
      assert.equal(session.restore(session.mask(text)), text, text);
    }
  });

  it("stays fast on long text", () => {
    const long = "9876543210 rahul@x.com OTP 123456 ".repeat(2_000);
    const started = performance.now();
    createMaskSession().mask(long);
    assert.ok(performance.now() - started < 1_000);
  });
});
