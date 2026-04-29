import { type ClassValue, clsx } from 'clsx';
import { twMerge } from 'tailwind-merge';

/**
 * Standard shadcn-svelte class-name utility.
 * Combines clsx (conditional classes) with tailwind-merge (dedupe conflicting Tailwind utilities).
 */
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

/**
 * Standard shadcn-svelte component prop types.
 * Mirrors the upstream `$lib/utils.js` exports that all shadcn-svelte components import.
 */
export type WithoutChild<T> = T extends { child?: any } ? Omit<T, 'child'> : T;
export type WithoutChildren<T> = T extends { children?: any } ? Omit<T, 'children'> : T;
export type WithoutChildrenOrChild<T> = WithoutChildren<WithoutChild<T>>;
export type WithElementRef<T, U extends HTMLElement = HTMLElement> = T & {
  ref?: U | null;
};
