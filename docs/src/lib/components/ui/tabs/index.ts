import Root from "./tabs.svelte";
import Content from "./tabs-content.svelte";
import List, { tabsListVariants, type TabsListVariant } from "./tabs-list.svelte";
import Trigger from "./tabs-trigger.svelte";
import PlatformTabs from "./platform-tabs.astro";
import type { PlatformTabItem } from "./platform-tabs.svelte";
import OsInstallTabs from "./os-install-tabs.astro";
import type {
	OsInstallPlatform,
	OsInstallVariant,
} from "./os-install-tabs.svelte";

export {
	Root,
	Content,
	List,
	Trigger,
	tabsListVariants,
	type TabsListVariant,
	//
	Root as Tabs,
	Content as TabsContent,
	List as TabsList,
	Trigger as TabsTrigger,
	//
	PlatformTabs,
	type PlatformTabItem,
	//
	OsInstallTabs,
	type OsInstallPlatform,
	type OsInstallVariant,
};
