import type { Config } from "../../config/index.js";
import { createStubTelephony } from "./stub.js";
import type { TelephonyProvider } from "./types.js";

export { createStubTelephony, type StubTelephony } from "./stub.js";
export type * from "./types.js";

export function createTelephony(config: Config["telephony"]): TelephonyProvider {
  switch (config.provider) {
    case "stub":
      return createStubTelephony();
  }
}
