import type { APIRoute } from 'astro';
import { auth } from './auth';

export const prerender = false;

/** Better Auth's endpoints: sign-in, sign-up, sessions, OAuth callbacks. */
export const ALL: APIRoute = ({ request }) => auth.handler(request);
