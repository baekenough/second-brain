import NextAuth from "next-auth";
import GitHub from "next-auth/providers/github";

/**
 * Auth.js v5 (next-auth@beta) configuration.
 *
 * Required env vars:
 *   AUTH_SECRET            — JWT signing secret  (openssl rand -base64 32)
 *   GITHUB_CLIENT_ID       — GitHub OAuth app client ID
 *   GITHUB_CLIENT_SECRET   — GitHub OAuth app client secret
 *   ALLOWED_GITHUB_USERS   — Comma-separated GitHub usernames that may sign in
 *                            (empty = allow all GitHub users — NOT recommended)
 *
 * GitHub OAuth callback URL: https://<domain>/api/auth/callback/github
 */
export const { auth, handlers, signIn, signOut } = NextAuth({
  providers: [
    GitHub({
      clientId: process.env.GITHUB_CLIENT_ID,
      clientSecret: process.env.GITHUB_CLIENT_SECRET,
    }),
  ],

  callbacks: {
    /**
     * Whitelist check — called after the OAuth provider returns a profile.
     * Returns true to allow sign-in, false or a redirect URL to deny.
     */
    async signIn({ profile }) {
      const raw = process.env.ALLOWED_GITHUB_USERS ?? "";
      if (!raw.trim()) {
        // Whitelist not configured: deny everyone for safety.
        // Set ALLOWED_GITHUB_USERS to allow specific users.
        return false;
      }
      const allowed = raw
        .split(",")
        .map((u) => u.trim().toLowerCase())
        .filter(Boolean);

      const login = (profile?.login as string | undefined)?.toLowerCase() ?? "";
      return allowed.includes(login);
    },

    /**
     * Session callback — expose github login in session for UI.
     */
    async session({ session, token }) {
      if (session.user && token.sub) {
        // Attach GitHub login (sub = GitHub user id, use name as display)
        (session.user as { githubLogin?: string }).githubLogin =
          (token.githubLogin as string | undefined) ?? session.user.name ?? "";
      }
      return session;
    },

    /**
     * JWT callback — persist GitHub login from profile into token.
     */
    async jwt({ token, profile }) {
      if (profile?.login) {
        token.githubLogin = profile.login;
      }
      return token;
    },

    /**
     * Authorization callback — called by middleware to decide if a request
     * may proceed. Returns true for authenticated users.
     */
    authorized({ auth: session }) {
      return !!session?.user;
    },
  },
});
