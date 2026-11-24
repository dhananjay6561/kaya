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

  it("never confuses a placeholder someone typed with one it issued", () => {
    const session = createMaskSession();
    const old = session.mask("I couldn't find [PHONE_1] anywhere");
    const real = session.mask("Priya is on 9123456780");
    assert.equal(real, "Priya is on [PHONE_1]");
    assert.doesNotMatch(
      old,
      /\[PHONE_1\]/,
      "the typed one is escaped, so it cannot be restored into a number",
    );
    assert.equal(session.restore(old), "I couldn't find [PHONE_1] anywhere");
    assert.equal(session.restore(real), "Priya is on 9123456780");
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

  describe("cases found in review", () => {
    const hides = (text: string, ...secrets: string[]) => {
      const out = masked(text);
      for (const secret of secrets)
        assert.ok(!out.includes(secret), `${secret} leaked from ${text} -> ${out}`);
      return out;
    };

    it("finds a number right after another number", () => {
      hides("Room 302 9876543210", "9876543210");
      hides("ref 12345 9876543210", "9876543210");
      hides("numbers: 98765 43210 91234 56780", "98765 43210", "91234 56780");
    });

    it("gives different PANs and IFSCs different placeholders", () => {
      const session = createMaskSession();
      const out = session.mask(
        "Rahul PAN ABCDE1234F, Priya PAN PQRST1234Z, HDFC0001234 vs ICIC0001234",
      );
      assert.equal(out, "Rahul PAN [PAN_1], Priya PAN [PAN_2], [IFSC_1] vs [IFSC_2]");
      assert.equal(
        session.restore(out),
        "Rahul PAN ABCDE1234F, Priya PAN PQRST1234Z, HDFC0001234 vs ICIC0001234",
      );
    });

    it("stays fast on hostile whitespace", () => {
      const started = performance.now();
      masked(`flat${" ".repeat(5_000)}x`);
      masked(`Flat${"\n".repeat(5_000)}`);
      masked(`house no${"\t".repeat(5_000)}#`);
      assert.ok(performance.now() - started < 1_000);
    });

    it("judges each repeat of an account number by its own context", () => {
      hides("Sent from 100123456789; account no is 100123456789", "account no is 100123456789");
    });

    it("hides UPI ids at the end of a sentence", () => {
      hides("pay me at rahul@ybl.", "rahul@ybl");
      hides("rahul@okhdfcbank.", "rahul@okhdfcbank");
      hides("9876543210@ybl.", "9876543210");
    });

    it("hides OTPs however they are written", () => {
      hides("OTP:482913", "482913");
      hides("otp482913", "482913");
      hides("OTP: 4 8 2 9 1 3", "4 8 2 9 1 3");
      hides("OTP is 482-913", "482-913");
      hides("Use 482913 to log in, it's your login code", "482913");
      hides("code hai 482913", "482913");
      const session = createMaskSession();
      assert.equal(
        session.mask("Here it is: 482913", { context: "what was that OTP?" }),
        "Here it is: [OTP_1]",
      );
    });

    it("hides landlines, odd spellings, other scripts and dotted cards", () => {
      hides("office 011-23456789", "23456789");
      hides("office 011 2345 6789", "2345 6789");
      hides("+91-9876-543-210", "9876-543-210");
      hides("+91 9876 543 210", "9876 543 210");
      hides("(+91) 98765-43210", "98765-43210");
      hides("नंबर ९८७६५४३२१० है", "९८७६५४३२१०");
      hides("full width ９８７６５４３２１０", "９８７６５４３２１０");
      hides("4111.1111.1111.1111", "4111.1111.1111.1111");
      hides("pan abcde1234f", "abcde1234f");
    });

    it("hides addresses written without a PIN code", () => {
      hides("C-12, Lajpat Nagar, New Delhi", "C-12", "Lajpat");
      hides("B-204, Sunshine Apartments, Sector 15, Gurugram", "B-204", "Sunshine");
      hides("12 MG Road, Sector 15, Gurugram", "MG Road");
      hides(
        "send here:\n12 MG Road, Gurugram 122001, call 9876543210",
        "MG Road",
        "122001",
        "9876543210",
      );
    });

    it("hides bank accounts named by the bank, or with spaces", () => {
      hides("HDFC 50100234567891", "50100234567891");
      hides("transfer to 50100234567891 at HDFC", "50100234567891");
      hides("a/c 5010 0234 5678 91", "5010 0234 5678 91");
    });

    it("hides vehicle and passport numbers", () => {
      hides("car DL 3C AB 1234 parked", "DL 3C AB 1234");
      hides("passport K1234567", "K1234567");
    });

    it("does not swallow sentences or ordinary numbers", () => {
      for (const text of [
        "Salary of 150000 is credited; the office is near Cyber Hub.",
        "In your code, set the port to 5432 and timeout 30000",
        "Rs 45000 and the promo code SAVE10",
        "the door 2 times",
        "shop 24x7",
        "it costs 1,23,456 rupees",
        "₹ 98765 total",
        "the OTP screen ships in 2026",
        "the OTP service costs Rs 4500 a month",
        "tracking id 123456789012 via Bluedart",
      ]) {
        assert.equal(masked(text), text, text);
      }
      assert.equal(masked("your PIN is 482913"), "your PIN is [OTP_1]");
    });

    it("restores placeholders the model reformatted, and only ones it issued", () => {
      const session = createMaskSession();
      session.mask("Rahul 9876543210");
      for (const form of [
        "[PHONE_1]",
        "[Phone_1]",
        "PHONE_1",
        "[PHONE 1]",
        "[ PHONE_1 ]",
        "[PHONE\\_1]",
      ]) {
        assert.equal(session.restore(`call ${form} now`), "call 9876543210 now", form);
      }
      assert.equal(session.restore("call [PHONE_2] now"), "call [PHONE_2] now");
    });
  });
});
