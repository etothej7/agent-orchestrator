import { createFileRoute } from "@tanstack/react-router";
import { WorkspaceEntry } from "../components/WorkspaceEntry";
export const Route = createFileRoute("/_shell/workspace")({ component: WorkspaceEntry });
