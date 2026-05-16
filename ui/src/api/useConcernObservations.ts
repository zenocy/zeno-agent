import { useQuery } from "@tanstack/react-query";
import type { ConcernObservationsResponse } from "../types";

// useConcernObservations is consumed by the Split modal — it loads the
// observation list for the source concern so the user can route each
// observation into one of the two new concerns. Cached briefly; the
// list is stable for the duration of the modal session.
export function useConcernObservations(id: string | null) {
  return useQuery<ConcernObservationsResponse>({
    queryKey: ["concerns", id, "observations"],
    queryFn: async () => {
      const r = await fetch(`/api/concerns/${id}/observations`);
      if (!r.ok) throw new Error(`/api/concerns/${id}/observations returned ${r.status}`);
      return r.json();
    },
    enabled: !!id,
    staleTime: 60 * 1000,
  });
}
