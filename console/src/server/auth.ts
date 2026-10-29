// Sign-in for the console in server mode, built on Better Auth. This is a
// starting point, not a fixed design: change the database, add providers or
// plugins (organizations, passkeys, SSO) here, and the rest of the console
// keeps working because it only asks Better Auth whether a session exists.
import { betterAuth } from 'better-auth';
import { APIError } from 'better-auth/api';
import { getMigrations } from 'better-auth/db/migration';
import { DatabaseSync } from 'node:sqlite';

const env = process.env;

/** Only these emails may create an account. */
const allowed = (env.CONSOLE_ALLOWED_EMAILS ?? '')
  .split(',')
  .map((e: string) => e.trim().toLowerCase())
  .filter(Boolean);

const github =
  env.GITHUB_CLIENT_ID && env.GITHUB_CLIENT_SECRET
    ? { github: { clientId: env.GITHUB_CLIENT_ID, clientSecret: env.GITHUB_CLIENT_SECRET } }
    : {};

/** What the sign-in page offers besides email and password. */
export const signInOptions = { github: 'github' in github };

export const auth = betterAuth({
  appName: 'Differently console',
  baseURL: env.BETTER_AUTH_URL,
  secret: env.BETTER_AUTH_SECRET,
  // SQLite through Node's built-in driver: one file, no native module. Swap in
  // Postgres, MySQL or Cloudflare D1 with any Better Auth database adapter.
  database: new DatabaseSync(env.CONSOLE_DB_PATH ?? './console.db'),
  emailAndPassword: { enabled: true, minPasswordLength: 8 },
  socialProviders: github,
  databaseHooks: {
    user: {
      create: {
        before: async (user) => {
          if (!allowed.includes(user.email.toLowerCase())) {
            throw new APIError('FORBIDDEN', { message: 'This email is not on the console allowlist (CONSOLE_ALLOWED_EMAILS).' });
          }
          return { data: user };
        },
      },
    },
  },
});

// Create the console's own tables (users, sessions, accounts) on start.
const { runMigrations } = await getMigrations(auth.options);
await runMigrations();
