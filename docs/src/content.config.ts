import { defineCollection } from 'astro:content';
import { glob } from 'astro/loaders';
import { z } from 'astro/zod';

const docs = defineCollection({
  loader: glob({ pattern: '**/*.{md,mdx}', base: './src/content/docs' }),
  schema: z.object({
    title: z.string(),
    description: z.string().optional(),
    order: z.number().optional(),
    // When true, the page is excluded from production builds (both as a route
    // and from the sidebar). Visible in `astro dev` so authors can preview.
    draft: z.boolean().default(false)
  })
});

export const collections = { docs };
