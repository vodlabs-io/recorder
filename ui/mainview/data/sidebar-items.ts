import {
  CircleArrowUp,
  Cpu,
  Film,
  Info,
  KeyRound,
  Settings2,
  Video,
} from "lucide-react"

// Named so the sidebar can hang the running/queued badge on this entry
// without matching a URL string twice.
const encodingItem = {
  title: "Encoding",
  url: "#/encoding",
  icon: Cpu,
};

export const sidebarItems = {
  navMain: [
    {
      title: "Dashboard",
      url: "#/dashboard",
      icon: Video,
    },
    {
      title: "VOD Archive",
      url: "#/vods",
      icon: Film,
    },
    encodingItem,
    {
      title: "Accounts",
      url: "#/accounts",
      icon: KeyRound,
    },
  ],
  navEncoding: encodingItem,
  // Its own top-level item (rendered between navMain and navSettings). The
  // sidebar shows a badge on it when an app or daemon update is available.
  navUpdate: {
    title: "Update",
    url: "#/update",
    icon: CircleArrowUp,
  },
  navSettings: [
    {
      title: "Settings",
      url: "#/settings/general",
      icon: Settings2,
      items: [
        {
          title: "General",
          url: "#/settings/general",
        },
        {
          title: "Files",
          url: "#/settings/files",
        },
        {
          title: "Recording",
          url: "#/settings/recording",
        },
        {
          title: "Processing",
          url: "#/settings/processing",
        },
      ],
    },
  ],
  navAbout: {
    title: "About",
    url: "#/about",
    icon: Info,
  },
}