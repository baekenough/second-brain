/**
 * Auth.js v5 catch-all route handler.
 * Handles: GET/POST /api/auth/signin, /api/auth/callback/github, /api/auth/signout, etc.
 */
import { handlers } from "@/auth";

export const { GET, POST } = handlers;
