import { createFileRoute } from "@tanstack/react-router";
import { UsageView } from "../components/usage/UsageView";

// Fork (Icosa2050): Usage is a normal page, not a Developer Mode one; only
// its experimental Windows tab stays behind Developer Mode (UsageView).
export const Route = createFileRoute("/_shell/usage")({ component: UsageView });
