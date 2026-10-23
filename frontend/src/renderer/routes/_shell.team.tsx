import { createFileRoute } from "@tanstack/react-router";
import { TeamView } from "../components/TeamView";
export const Route = createFileRoute("/_shell/team")({ component: TeamView });
