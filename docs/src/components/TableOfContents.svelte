<script lang="ts">
  import { onMount } from 'svelte';

  type Heading = {
    slug: string;
    text: string;
    depth: number;
  };

  let { headings = [] }: { headings?: Heading[] } = $props();
  let activeSlug = $state('');

  // Filter to h2 and h3 only
  const tocHeadings = $derived(headings.filter(h => h.depth >= 2 && h.depth <= 3));

  onMount(() => {
    const elements = tocHeadings
      .map(h => document.getElementById(h.slug))
      .filter((el): el is HTMLElement => el !== null);

    if (elements.length === 0) return;

    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (entry.isIntersecting) {
            activeSlug = entry.target.id;
            break;
          }
        }
      },
      { rootMargin: '0px 0px -80% 0px', threshold: 0 }
    );

    for (const el of elements) {
      observer.observe(el);
    }

    return () => observer.disconnect();
  });
</script>

{#if tocHeadings.length > 0}
  <nav class="hidden xl:block w-56 shrink-0 rounded border border-border bg-muted shadow-low px-3 py-3" aria-label="On this page">
    <div class="sticky top-8">
      <p class="text-sm font-medium text-foreground mb-3">On this page</p>
      <ul class="space-y-1 text-sm list-none p-0 m-0">
        {#each tocHeadings as heading}
          <li class="{heading.depth === 3 ? 'ml-3' : ''}">
            <a
              href="#{heading.slug}"
              class="block py-0.5 no-underline
                {activeSlug === heading.slug
                  ? 'text-foreground font-medium'
                  : 'text-muted-foreground hover:text-foreground'}"
            >
              {heading.text}
            </a>
          </li>
        {/each}
      </ul>
    </div>
  </nav>
{/if}
