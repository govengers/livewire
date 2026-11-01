// The livewire Vue 3 composables (D21): live reads with an identity-keyed snapshot, live lists
// with filter, sort and page state, the connection status and presence. The generator copies
// this file next to runtime.ts. Vue only, no component library.

import {
  computed,
  hasInjectionContext,
  inject,
  onScopeDispose,
  ref,
  shallowRef,
  toValue,
  watch,
  type ComputedRef,
  type InjectionKey,
  type MaybeRefOrGetter,
  type Plugin,
  type Ref,
  type ShallowRef,
} from 'vue'
import {
  itemID,
  wireParameters,
  type ApiError,
  type ListResponse,
  type LiveConnection,
  type LivePair,
  type LiveState,
  type LiveStatus,
  type LiveSubscription,
  type LiveTarget,
  type NetworkError,
  type PresenceItem,
  type PresenceRegistration,
} from './runtime'

/** The key the application provides its current pair under (livePlugin does). */
export const livePairKey: InjectionKey<Readonly<Ref<LivePair>>> = Symbol('livewire pair')

/**
 * A Vue plugin that provides the current pair (D66): app.use(livePlugin(pair)), where pair is a
 * ref the application replaces with a new pair (createLivePair) on every identity change. Every
 * composable follows it: it ends its subscriptions on the old pair and subscribes on the new.
 */
export function livePlugin(pair: Readonly<Ref<LivePair>>): Plugin {
  return { install: (app) => void app.provide(livePairKey, pair) }
}

/** The provided pair reference. */
export function useLivePair(): Readonly<Ref<LivePair>> {
  const pair = hasInjectionContext() ? inject(livePairKey, undefined) : undefined
  if (pair === undefined) throw new Error('livewire: no pair provided; app.use(livePlugin(pair)) or pass options.connection')
  return pair
}

/** The current connection: the one given (a connection, a ref or a getter of one), else the provided pair's. */
export function useLiveConnection(connection?: MaybeRefOrGetter<LiveConnection>): ComputedRef<LiveConnection> {
  if (connection !== undefined) return computed(() => toValue(connection))
  const pair = useLivePair()
  return computed(() => pair.value.connection)
}

/** The part of the Web Storage API the snapshots need; localStorage by default. */
export interface SnapshotStorage {
  readonly length: number
  key(index: number): string | null
  getItem(key: string): string | null
  setItem(key: string, value: string): void
  removeItem(key: string): void
}

export interface UseLiveOptions {
  /** The connection to use, or a ref or getter the composable follows; the provided pair's by default. */
  connection?: MaybeRefOrGetter<LiveConnection>
  /**
   * Keep the last data in storage and show it, stale, before the socket answers (D21). The
   * snapshot belongs to the identity of the connection that delivered it: without one nothing is
   * kept, and another identity clears every snapshot of the one before.
   */
  cache?: boolean
  storage?: SnapshotStorage
}

export interface LiveRead<Data> {
  data: ShallowRef<Data | undefined>
  error: ShallowRef<ApiError | NetworkError | undefined>
  /** True until the first push and whenever the socket is not open. */
  stale: Ref<boolean>
  /** No data yet and no error: the one condition a skeleton should show for. */
  loading: ComputedRef<boolean>
  /** The ids an optimistic call has not settled yet (D36). */
  pending: ShallowRef<readonly string[]>
  /** The stored filter an inline filter was registered as (D13), for a link to this view. */
  filterId: Ref<string | undefined>
  /** Subscribes again, after an error ended the subscription. */
  retry(): void
}

const snapshotPrefix = 'livewire:'
const identityKey = `${snapshotPrefix}identity`

function defaultStorage(): SnapshotStorage | undefined {
  return typeof localStorage === 'undefined' ? undefined : localStorage
}

/**
 * Reads the snapshot under key for the identity; a snapshot of another identity is never shown:
 * a new identity clears every livewire snapshot first, and nobody ("") has none (D21). A storage
 * that fails (full, private mode, unparsable) is a miss: the view subscribes without a snapshot.
 */
function readSnapshot(storage: SnapshotStorage, identity: string, key: string): unknown {
  try {
    forgetOtherIdentities(storage, identity)
    if (identity === '') return undefined
    const stored = storage.getItem(key)
    return stored === null ? undefined : JSON.parse(stored)
  } catch {
    return undefined
  }
}

/** Writes the snapshot of a state under the identity of the connection that delivered it (D66). */
function writeSnapshot(storage: SnapshotStorage, identity: string, key: string, data: unknown): void {
  try {
    forgetOtherIdentities(storage, identity)
    if (identity !== '') storage.setItem(key, JSON.stringify(data))
  } catch {
    // a full storage keeps no snapshot: the next push tries again
  }
}

function forgetOtherIdentities(storage: SnapshotStorage, identity: string): void {
  if (storage.getItem(identityKey) === identity) return
  const keys: string[] = []
  for (let index = 0; index < storage.length; index++) {
    const key = storage.key(index)
    if (key?.startsWith(snapshotPrefix) === true) keys.push(key)
  }
  for (const key of keys) storage.removeItem(key)
  storage.setItem(identityKey, identity)
}

function snapshotKey(identity: string, target: string, parameters: Record<string, string>): string {
  return `${snapshotPrefix}${identity}:${target}:${JSON.stringify(Object.entries(parameters).sort())}`
}

/** Calls listener with the status of the current connection, following it when it is replaced; the result stops. */
function followStatus(connection: ComputedRef<LiveConnection>, listener: (status: LiveStatus) => void): () => void {
  let stop = (): void => {}
  const stopWatching = watch(connection, (current) => {
    stop()
    stop = current.onStatus(listener)
  }, { immediate: true })
  return () => {
    stopWatching()
    stop()
  }
}

/** The arguments after the target: the parameters, optional when the read needs none, then the options. */
type ReadArguments<Parameters, Options> = Record<string, never> extends Parameters
  ? [parameters?: MaybeRefOrGetter<Parameters>, options?: Options]
  : [parameters: MaybeRefOrGetter<Parameters>, options?: Options]

/**
 * Subscribes to a live read for as long as the calling scope lives. A change of the reactive
 * parameters subscribes again and keeps the old data, stale, meanwhile. The connection's end
 * (D66) drops the data and the pending changes at once; a new connection (a new pair) drops them
 * too and subscribes on it.
 */
export function useLive<Parameters extends object, Data>(target: LiveTarget<Parameters, Data>, ...rest: ReadArguments<Parameters, UseLiveOptions>): LiveRead<Data> {
  const [parameters, options = {}] = rest as [MaybeRefOrGetter<Parameters>?, UseLiveOptions?]
  return liveRead(target, parameters, options, useLiveConnection(options.connection))
}

function liveRead<Parameters extends object, Data>(
  target: LiveTarget<Parameters, Data>,
  parameters: MaybeRefOrGetter<Parameters> | undefined,
  options: UseLiveOptions,
  connection: ComputedRef<LiveConnection>,
): LiveRead<Data> {
  const storage = options.cache === true ? (options.storage ?? defaultStorage()) : undefined
  const data = shallowRef<Data>()
  const error = shallowRef<ApiError | NetworkError>()
  const stale = ref(true)
  const pending = shallowRef<readonly string[]>([])
  const filterId = ref<string>()
  let subscription: LiveSubscription | undefined
  let subscribedOn: LiveConnection | undefined
  // The callbacks of a replaced subscription change nothing: only the newest token's do.
  let token: object | undefined

  const forget = (): void => {
    data.value = undefined
    pending.value = []
    filterId.value = undefined
    stale.value = true
  }
  const current = (): Parameters => (toValue(parameters) ?? {}) as Parameters
  const subscribe = (): void => {
    subscription?.unsubscribe()
    const live = connection.value
    if (subscribedOn !== undefined && subscribedOn !== live) forget()
    subscribedOn = live
    const wire = wireParameters(current())
    if (storage !== undefined) {
      const cached = readSnapshot(storage, live.identity(), snapshotKey(live.identity(), target.target, wire))
      if (cached !== undefined) data.value = cached as Data
    }
    error.value = undefined
    stale.value = true
    const owned = {}
    token = owned
    subscription = live.subscribe(target, current(), {
      next: (state: LiveState<Data>) => {
        if (token !== owned) return
        data.value = state.data
        pending.value = state.pending
        filterId.value = state.filterId
        error.value = undefined
        stale.value = live.status !== 'open'
        // The state belongs to the identity of the connection that delivered it.
        if (storage !== undefined) writeSnapshot(storage, state.identity, snapshotKey(state.identity, target.target, wire), state.data)
      },
      error: (refusal) => {
        if (token === owned) error.value = refusal
      },
      end: () => {
        if (token === owned) forget()
      },
    })
  }
  watch(() => [connection.value, JSON.stringify(wireParameters(current()))], subscribe, { immediate: true })
  const stopStatus = followStatus(connection, (status: LiveStatus) => {
    if (status !== 'open') stale.value = true
  })
  onScopeDispose(() => {
    token = undefined
    subscription?.unsubscribe()
    subscription = undefined
    stopStatus()
  })
  return {
    data, error, stale, pending, filterId,
    loading: computed(() => error.value === undefined && stale.value && data.value === undefined),
    retry: () => {
      error.value = undefined
      subscription?.retry()
    },
  }
}

/** The reserved parameters of a filterable list read the list state drives. */
interface ListQuery {
  filter?: unknown
  sort?: readonly string[]
  limit?: number
  offset?: number
  after?: string
}

export interface UseLiveListOptions<Query extends ListQuery> {
  /** The connection to use, or a ref or getter the list follows; the provided pair's by default. */
  connection?: MaybeRefOrGetter<LiveConnection>
  /** The initial filter document. */
  filter?: Query['filter']
  /** The initial sort, "-field" for descending; none is the resource's default sort. */
  sort?: Query['sort']
  /** The page size; none is the resource's MaximumLimit. */
  limit?: number
  /** offset (default): one page at a time, `page` moves it; cursor: pages tiled with loadMore() (D38). */
  mode?: 'offset' | 'cursor'
  /** The read's other parameters, if any. */
  parameters?: MaybeRefOrGetter<Omit<Query, keyof ListQuery | 'filterId'>>
}

export interface LiveList<Query extends ListQuery, Item> {
  items: ComputedRef<Item[]>
  /** The total under scope and filter (at most CountCap + 1 under a cap, D14). */
  total: ComputedRef<number | undefined>
  /** The refusal that ended a page, or the failed read of a page's next cursor (D64); retry() starts both again. */
  error: ComputedRef<ApiError | NetworkError | undefined>
  stale: ComputedRef<boolean>
  loading: ComputedRef<boolean>
  pending: ComputedRef<readonly string[]>
  filter: Ref<Query['filter']>
  sort: Ref<Query['sort']>
  limit: Ref<number | undefined>
  /** The page in offset mode, from 1; a filter or sort change goes back to 1. */
  page: Ref<number>
  /** The pages in offset mode, undefined before the first push or without a limit. */
  pages: ComputedRef<number | undefined>
  /** Whether loadMore() has a page to add (cursor mode). */
  hasMore: ComputedRef<boolean>
  /** Adds the next page below the loaded ones (cursor mode). */
  loadMore(): void
  retry(): void
}

/** One live page of a list: offset mode has one, cursor mode tiles several. */
interface Tile<Item> {
  after: string | undefined
  /** The cursor after the page: what its last snapshot or the last read of the page answered (a diff carries none). */
  next: Ref<string | undefined>
  data: ShallowRef<ListResponse<Item> | undefined>
  error: ShallowRef<ApiError | NetworkError | undefined>
  refreshError: ShallowRef<ApiError | NetworkError | undefined>
  /** Counts the reads of the page's next cursor and the snapshots; only the answer of the newest read counts. */
  refreshes: number
  pending: ShallowRef<readonly string[]>
  received: Ref<boolean>
  subscription: LiveSubscription | undefined
}

/**
 * A live list with its filter, sort and page state (D21). In cursor mode (D38, D64) the first
 * tile is the first page and loadMore() subscribes the page after the last tile's `next`. Every
 * snapshot of a tile carries its `next`, which is taken and outdates any read of the page on its
 * way; when a diff changes its rows or its last row (a sort value moved), the page is read again
 * for its current `next`, the newest read wins, and the tile below follows it, so no row falls
 * between two tiles. A new connection (a new pair) starts over; the connection's end drops the
 * pages.
 */
export function useLiveList<Query extends ListQuery, Item>(target: LiveTarget<Query, ListResponse<Item>>, options: UseLiveListOptions<Query> = {}): LiveList<Query, Item> {
  const connection = useLiveConnection(options.connection)
  const filter = ref(options.filter) as Ref<Query['filter']>
  const sort = ref(options.sort) as Ref<Query['sort']>
  const limit = ref(options.limit)
  const page = ref(1)
  const tiles = shallowRef<Tile<Item>[]>([])
  const open = ref(false)
  const cursor = options.mode === 'cursor'
  let disposed = false

  // The page size the server applied to the limit asked for (D14: clamped to MaximumLimit), else
  // that limit; a new limit forgets it.
  const appliedLimit = ref<number>()
  const pageSize = (): number | undefined => appliedLimit.value ?? limit.value
  const queryOf = (after: string | undefined): Query => {
    const query: Record<string, unknown> = { ...(toValue(options.parameters) ?? {}) }
    if (filter.value !== undefined) query['filter'] = filter.value
    if (sort.value !== undefined && sort.value.length > 0) query['sort'] = sort.value
    if (limit.value !== undefined) query['limit'] = limit.value
    if (cursor) {
      if (after !== undefined) query['after'] = after
    } else if (page.value > 1) {
      const size = pageSize()
      if (size !== undefined) query['offset'] = (page.value - 1) * size
    }
    return query as Query
  }

  const startTile = (after: string | undefined, position: number): Tile<Item> => {
    const tile: Tile<Item> = {
      after, next: ref(), data: shallowRef(), error: shallowRef(), refreshError: shallowRef(), refreshes: 0,
      pending: shallowRef([]), received: ref(false), subscription: undefined,
    }
    tile.subscription = connection.value.subscribe(target, queryOf(after), {
      next: (state) => {
        if (disposed || tiles.value[position] !== tile) return
        const before = tile.data.value
        tile.data.value = state.data
        tile.pending.value = state.pending
        tile.error.value = undefined
        tile.received.value = true
        if (position === 0 && state.cause !== 'optimistic') appliedLimit.value = state.data.limit
        if (!cursor) return
        if (state.cause === 'snapshot') {
          // A snapshot carries the page's next: take it, over any read of the page still on its way.
          tile.refreshes++
          tile.refreshError.value = undefined
          tile.next.value = state.data.next
          follow(position, state.data.next)
        } else if (state.cause === 'change' && before !== undefined && boundaryMoved(before, state.data)) {
          refreshNext(position)
        }
      },
      error: (refusal) => {
        tile.error.value = refusal
      },
      end: () => {
        tile.data.value = undefined
        tile.pending.value = []
        tile.next.value = undefined
        tile.received.value = false
      },
    })
    return tile
  }

  // The tile below a tile starts at its `next`: a tile that moved gets a new one there, which
  // in turn moves the tile below it once its first page arrived.
  const follow = (position: number, next: string | undefined): void => {
    const below = tiles.value[position + 1]
    if (disposed || below === undefined || below.after === next) return
    const replaced = [...tiles.value]
    const dropped = next === undefined ? replaced.splice(position + 1) : [below]
    if (next !== undefined) replaced[position + 1] = startTile(next, position + 1)
    tiles.value = replaced
    for (const tile of dropped) tile.subscription?.unsubscribe()
  }

  // A diff carries no `next`: a tile whose boundary moved reads its page again for it.
  const refreshNext = (position: number): void => {
    const tile = tiles.value[position]
    if (tile === undefined) return
    const refresh = ++tile.refreshes
    connection.value.query(target, queryOf(tile.after)).then(
      (fresh) => {
        if (disposed || tiles.value[position] !== tile || refresh !== tile.refreshes) return
        tile.refreshError.value = undefined
        tile.next.value = fresh.next
        follow(position, fresh.next)
      },
      (failure: ApiError | NetworkError) => {
        if (!disposed && tiles.value[position] === tile && refresh === tile.refreshes) tile.refreshError.value = failure
      },
    )
  }

  const stopTiles = (): void => {
    const stopped = tiles.value
    tiles.value = []
    for (const tile of stopped) tile.subscription?.unsubscribe()
  }
  const restart = (): void => {
    stopTiles()
    tiles.value = [startTile(undefined, 0)]
  }
  watch(limit, () => {
    appliedLimit.value = undefined
  })
  // A new connection starts over too.
  watch(() => [connection.value, JSON.stringify([toValue(options.parameters), filter.value, sort.value, limit.value])], () => {
    if (page.value !== 1 && !cursor) page.value = 1 // the page watcher restarts
    else restart()
  })
  watch(page, restart, { immediate: true })
  const stopStatus = followStatus(connection, (status) => {
    open.value = status === 'open'
  })
  onScopeDispose(() => {
    disposed = true
    stopTiles()
    stopStatus()
  })

  const items = computed(() => {
    const seen = new Set<string>()
    const merged: Item[] = []
    for (const tile of tiles.value) {
      for (const item of tile.data.value?.items ?? []) {
        const id = itemID(item)
        if (!seen.has(id)) {
          seen.add(id)
          merged.push(item)
        }
      }
    }
    return merged
  })
  const first = computed(() => tiles.value[0])
  const error = computed(() => {
    for (const tile of tiles.value) {
      const failure = tile.error.value ?? tile.refreshError.value
      if (failure !== undefined) return failure
    }
    return undefined
  })
  const stale = computed(() => !open.value || tiles.value.some((tile) => !tile.received.value))
  const total = computed(() => first.value?.data.value?.total)
  return {
    items, total, error, stale, filter, sort, limit, page,
    loading: computed(() => error.value === undefined && first.value?.data.value === undefined),
    pending: computed(() => tiles.value.flatMap((tile) => tile.pending.value)),
    pages: computed(() => {
      const size = pageSize()
      return total.value === undefined || size === undefined || size === 0 ? undefined : Math.max(1, Math.ceil(total.value / size))
    }),
    hasMore: computed(() => cursor && tiles.value.at(-1)?.next.value !== undefined),
    loadMore: () => {
      const next = tiles.value.at(-1)?.next.value
      if (cursor && next !== undefined) tiles.value = [...tiles.value, startTile(next, tiles.value.length)]
    },
    retry: () => {
      tiles.value.forEach((tile, position) => {
        if (tile.error.value !== undefined) {
          tile.error.value = undefined
          tile.subscription?.retry()
        }
        if (tile.refreshError.value !== undefined) {
          tile.refreshError.value = undefined
          refreshNext(position)
        }
      })
    },
  }
}

/** Whether a page's boundary moved: its rows changed, or its last row (whose sort values the next cursor holds) did. */
function boundaryMoved(before: ListResponse<unknown>, after: ListResponse<unknown>): boolean {
  const changedRows = before.items.length !== after.items.length || before.items.some((item, index) => itemID(item) !== itemID(after.items[index]))
  return changedRows || JSON.stringify(before.items.at(-1)) !== JSON.stringify(after.items.at(-1))
}

/** The current connection's status, for an online indicator; it follows a new pair. */
export function useLiveStatus(connection?: MaybeRefOrGetter<LiveConnection>): Ref<LiveStatus> {
  const current = useLiveConnection(connection)
  const status = ref<LiveStatus>(current.value.status)
  onScopeDispose(followStatus(current, (changed) => {
    status.value = changed
  }))
  return status
}

export interface LivePresence<State> {
  presences: ComputedRef<PresenceItem<State>[]>
  /** The refusal of the presence read or of a join; a leave's is dropped (D37). */
  error: ComputedRef<ApiError | NetworkError | undefined>
  stale: Ref<boolean>
}

/**
 * Joins the record of a presence read with the reactive state and subscribes to who is there
 * (D37), for as long as the calling scope lives: a state change joins again (an update), a
 * change of the parameters (by value, so a mutated parameters object counts) or of the
 * connection (a new pair) leaves the old record and joins the new one, the end of the scope
 * leaves. Two components on one record share the socket's presence; it ends with the last.
 */
export function useLivePresence<Parameters extends object, State>(
  target: LiveTarget<Parameters, ListResponse<PresenceItem>>,
  parameters: MaybeRefOrGetter<Parameters>,
  state: MaybeRefOrGetter<State>,
  options: UseLiveOptions = {},
): LivePresence<State> {
  const connection = useLiveConnection(options.connection)
  const live = liveRead(target, parameters, options, connection)
  const presenceError = shallowRef<ApiError | NetworkError>()
  let registration: PresenceRegistration | undefined
  watch(
    () => [connection.value, JSON.stringify(wireParameters(toValue(parameters)))],
    () => {
      registration?.leave()
      presenceError.value = undefined
      registration = connection.value.join(target, toValue(parameters), toValue(state), {
        onError: (failure) => {
          presenceError.value = failure
        },
        onJoined: () => {
          presenceError.value = undefined
        },
      })
    },
    { immediate: true },
  )
  watch(() => JSON.stringify(toValue(state)), () => registration?.update(toValue(state)))
  onScopeDispose(() => {
    registration?.leave()
    registration = undefined
  })
  return {
    presences: computed(() => (live.data.value?.items ?? []) as PresenceItem<State>[]),
    error: computed(() => live.error.value ?? presenceError.value),
    stale: live.stale,
  }
}
