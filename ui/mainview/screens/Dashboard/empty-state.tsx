import { Plus, Users } from "lucide-react"
import { Button } from "@/components/ui/button"
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty"
import AddStreamer from "../AddStreamer"

// Stands in for the whole table UI until the first streamer is added: there is
// nothing to search, filter or paginate yet, so the only thing worth offering
// is the Add dialog the toolbar would otherwise hold.
export function DashboardEmpty() {
  return (
    // max-w-table keeps the panel on the same column as the table it replaces.
    <div className="mx-auto flex w-full max-w-table flex-col px-4 py-16">
      <Empty className="border">
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <Users />
          </EmptyMedia>
          <EmptyTitle>No streamers yet</EmptyTitle>
          <EmptyDescription>
            Add a streamer to start monitoring their channel. Recording starts
            automatically whenever they go live.
          </EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <AddStreamer
            trigger={
              <Button>
                <Plus />
                Add Streamer
              </Button>
            }
          />
        </EmptyContent>
      </Empty>
    </div>
  )
}
