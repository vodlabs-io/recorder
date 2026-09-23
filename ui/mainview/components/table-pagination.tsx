import {
  Pagination,
  PaginationContent,
  PaginationEllipsis,
  PaginationItem,
  PaginationLink,
  PaginationNext,
  PaginationPrevious,
} from "@/components/ui/pagination"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"

export const PAGE_SIZES = [10, 25, 50, 100] as const

export type PageItem = number | "ellipsis-start" | "ellipsis-end"

const PAGE_SLOTS = 7

// pageItems lists the 1-based page buttons to draw for `page` of `count`. Past
// PAGE_SLOTS pages it always fills exactly PAGE_SLOTS slots (first, last, the
// current page's neighbours and ellipses), so Previous/Next don't shift as the
// user pages through.
export function pageItems(page: number, count: number): PageItem[] {
  if (count <= PAGE_SLOTS) {
    return Array.from({ length: count }, (_, i) => i + 1)
  }
  const edge = PAGE_SLOTS - 2
  if (page < edge) {
    return [...range(1, edge), "ellipsis-end", count]
  }
  if (page > count - edge + 1) {
    return [1, "ellipsis-start", ...range(count - edge + 1, count)]
  }
  return [1, "ellipsis-start", page - 1, page, page + 1, "ellipsis-end", count]
}

function range(from: number, to: number): number[] {
  return Array.from({ length: to - from + 1 }, (_, i) => from + i)
}

// TablePagination is a table footer: the row count, a rows-per-page picker and
// numbered page buttons. Controls that have nothing to offer are left out: the
// picker when every row fits on the smallest page, the buttons when there is
// only one page.
export function TablePagination({
  pageIndex,
  pageCount,
  pageSize,
  rowCount,
  totalCount,
  noun,
  onPageChange,
  onPageSizeChange,
}: {
  pageIndex: number
  pageCount: number
  pageSize: number
  // Rows left after filtering, and before it.
  rowCount: number
  totalCount: number
  noun: string
  onPageChange: (pageIndex: number) => void
  onPageSizeChange: (pageSize: number) => void
}) {
  const page = pageIndex + 1
  const label = `${noun}${totalCount === 1 ? "" : "s"}`
  const summary =
    rowCount === totalCount
      ? `${totalCount} ${label}`
      : `${rowCount} of ${totalCount} ${label}`

  return (
    <div className="flex items-center gap-4 py-4">
      <div className="flex-1 text-muted-foreground tabular-nums">{summary}</div>
      {rowCount > PAGE_SIZES[0] && (
        <div className="flex items-center gap-2">
          <label
            htmlFor="rows-per-page"
            className="whitespace-nowrap text-muted-foreground"
          >
            Rows per page
          </label>
          <Select
            value={String(pageSize)}
            onValueChange={(value) => value && onPageSizeChange(Number(value))}
          >
            <SelectTrigger id="rows-per-page" className="w-20">
              <SelectValue />
            </SelectTrigger>
            <SelectContent align="start">
              {PAGE_SIZES.map((size) => (
                <SelectItem key={size} value={String(size)}>
                  {size}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      )}
      {pageCount > 1 && (
        <Pagination className="mx-0 w-auto">
          <PaginationContent>
            <PaginationItem>
              <PaginationPrevious
                onClick={() => onPageChange(pageIndex - 1)}
                disabled={page <= 1}
              />
            </PaginationItem>
            {pageItems(page, pageCount).map((item) =>
              typeof item === "number" ? (
                <PaginationItem key={item}>
                  <PaginationLink
                    isActive={item === page}
                    aria-label={`Page ${item}`}
                    onClick={() => onPageChange(item - 1)}
                  >
                    {item}
                  </PaginationLink>
                </PaginationItem>
              ) : (
                <PaginationItem key={item}>
                  <PaginationEllipsis />
                </PaginationItem>
              ),
            )}
            <PaginationItem>
              <PaginationNext
                onClick={() => onPageChange(pageIndex + 1)}
                disabled={page >= pageCount}
              />
            </PaginationItem>
          </PaginationContent>
        </Pagination>
      )}
    </div>
  )
}
