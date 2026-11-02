import * as React from "react"
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarRail,
  SidebarGroup,
  SidebarMenu,
  SidebarMenuBadge,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarMenuSub,
  SidebarMenuSubButton,
  SidebarMenuSubItem,
} from "@/components/ui/sidebar"
import { useLocation } from 'react-router-dom'
import { sidebarItems } from "@/data/sidebar-items";
import { appStore } from "@/state/app-state";
import { selectUpdateAvailable } from "@/state/update";
import { selectStorageNeedsAttention } from "@/components/storage-space";
import { expiredLogins } from "@shared/daemon/logins";
import { getDaemonClient } from "@/lib/daemon";
import { LogOut } from "lucide-react";

// AttentionDot marks a tab with something to deal with. Yellow whatever the
// severity: a red dot means recording.
function AttentionDot() {
  return <span className="ml-auto inline-flex h-2 w-2 shrink-0 rounded-full bg-yellow-500" />
}

export function AppSidebar({ ...props }: React.ComponentProps<typeof Sidebar>) {

  const location = useLocation();

  // Badge on the Update tab when either half has a newer version in hand.
  // Shared with the Update screen so the two can't disagree.
  const updateAvailable = appStore(selectUpdateAvailable);
  // Count on the Encoding tab of the jobs running or still waiting.
  const encodePending = appStore((s) => s.encode.pending);
  // Dot on Settings → Files when a storage root is low, full or unreachable.
  // The settings sub-menu is always expanded, so the parent needs no copy.
  const storageAttention = appStore(selectStorageNeedsAttention);
  // Dot on Accounts when a stored login needs signing in again.
  const loginExpired = appStore((s) => expiredLogins(s.accounts).length > 0);
  const encodingURL = sidebarItems.navEncoding.url;
  const updateItem = sidebarItems.navUpdate;
  const aboutItem = sidebarItems.navAbout;
  const client = getDaemonClient();

  return (
    <Sidebar collapsible="offcanvas" {...props}>
      <SidebarContent>
        <SidebarGroup>
          <SidebarMenu>
            {sidebarItems.navMain.map((item) => (
              <SidebarMenuItem key={item.title}>
                  <SidebarMenuButton
                    className="cursor-default"
                    tooltip={item.title}
                    isActive = {location.pathname == item.url.substring(1)}
                    render={
                      <a href={item.url}>
                        {item.icon && <item.icon />}
                        <span>{item.title}</span>
                        {item.url === sidebarItems.navAccounts.url && loginExpired && <AttentionDot />}
                      </a>
                    }
                  />
                  {item.url === encodingURL && encodePending > 0 && (
                    // SidebarMenuBadge recolors its text on hover and on the
                    // active row, which on a filled pill turns the count
                    // invisible; pin the foreground in both states.
                    <SidebarMenuBadge className="bg-primary text-primary-foreground peer-hover/menu-button:text-primary-foreground peer-data-active/menu-button:text-primary-foreground">
                      {encodePending}
                    </SidebarMenuBadge>
                  )}
              </SidebarMenuItem>
            ))}
            <SidebarMenuItem key={updateItem.title}>
              <SidebarMenuButton
                className="cursor-default"
                tooltip={updateItem.title}
                isActive = {location.pathname == updateItem.url.substring(1)}
                render={
                  <a href={updateItem.url}>
                    <updateItem.icon />
                    <span>{updateItem.title}</span>
                    {updateAvailable && <AttentionDot />}
                  </a>
                }
              />
            </SidebarMenuItem>
            {sidebarItems.navSettings.map((item) => (
              <SidebarMenuItem key={item.title}>
                  <SidebarMenuButton
                    className="cursor-default"
                    tooltip={item.title}
                    render={
                      <a href={item.url}>
                        {item.icon && <item.icon />}
                        <span>{item.title}</span>
                      </a>
                    }
                  />
                  <SidebarMenuSub>
                    {item.items?.map((subItem) => (
                      <SidebarMenuSubItem key={subItem.title}>
                        <SidebarMenuSubButton
                          className="cursor-default"
                          isActive = {location.pathname == subItem.url.substring(1)}
                          render={
                            <a href={subItem.url}>
                              <span>{subItem.title}</span>
                              {subItem.url.endsWith("/files") && storageAttention && <AttentionDot />}
                            </a>
                          }
                        />
                      </SidebarMenuSubItem>
                    ))}
                  </SidebarMenuSub>
              </SidebarMenuItem>
            ))}
            <SidebarMenuItem key={aboutItem.title}>
              <SidebarMenuButton
                className="cursor-default"
                tooltip={aboutItem.title}
                isActive = {location.pathname == aboutItem.url.substring(1)}
                render={
                  <a href={aboutItem.url}>
                    <aboutItem.icon />
                    <span>{aboutItem.title}</span>
                  </a>
                }
              />
            </SidebarMenuItem>
          </SidebarMenu>
        </SidebarGroup>
      </SidebarContent>
      <SidebarFooter>
        {client.capabilities.webSession && (
          <SidebarMenu>
            <SidebarMenuItem>
              <SidebarMenuButton
                className="cursor-default"
                tooltip="Sign out"
                onClick={() => void client.signOut()}
              >
                <LogOut />
                <span>Sign out</span>
              </SidebarMenuButton>
            </SidebarMenuItem>
          </SidebarMenu>
        )}
      </SidebarFooter>
      <SidebarRail />
    </Sidebar>
  )
}
