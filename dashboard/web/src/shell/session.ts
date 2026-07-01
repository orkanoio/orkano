import { createContext, useContext } from "react";

// Session is the signed-in identity the shell provides to every screen — the
// oidc flag drives StepUpForm's branch (IdP round-trip vs password+TOTP).
export interface Session {
  username: string;
  oidc: boolean;
}

export const SessionContext = createContext<Session | null>(null);

export function useSession(): Session {
  const session = useContext(SessionContext);
  if (!session) {
    throw new Error("useSession called outside the signed-in shell");
  }
  return session;
}
