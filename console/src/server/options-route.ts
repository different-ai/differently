import type { APIRoute } from 'astro';
import { signInOptions } from './auth';

export const prerender = false;

export const GET: APIRoute = () => Response.json(signInOptions);
