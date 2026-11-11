---- MODULE share ----
\* One shared mysqld. A test holds start.lock, then either joins the live
\* run or creates it. The test takes clients.lock before it releases start.lock.
\* The supervisor stops only from grace, with no holders, and with start.lock free.

EXTENDS FiniteSets, Integers

CONSTANTS Tests

VARIABLES startLock, holders, server, current, supervisor

TypeOK ==
  /\ startLock \in {"none"} \union Tests
  /\ holders \subseteq Tests
  /\ server \in {"down", "up"}
  /\ current \in {"none", "run"}
  /\ supervisor \in {"absent", "wait", "grace", "dead"}

Init ==
  /\ startLock = "none"
  /\ holders = {}
  /\ server = "down"
  /\ current = "none"
  /\ supervisor = "absent"

LockStart(t) ==
  /\ startLock = "none"
  /\ startLock' = t
  /\ UNCHANGED <<holders, server, current, supervisor>>

Join(t) ==
  /\ startLock = t
  /\ current = "run"
  /\ server = "up"
  /\ holders' = holders \union {t}
  /\ startLock' = "none"
  /\ UNCHANGED <<server, current, supervisor>>

Create(t) ==
  /\ startLock = t
  /\ current = "none"
  /\ server = "down"
  /\ supervisor = "absent"
  /\ current' = "run"
  /\ server' = "up"
  /\ supervisor' = "wait"
  /\ holders' = holders \union {t}
  /\ startLock' = "none"

Release(t) ==
  /\ t \in holders
  /\ holders' = holders \ {t}
  /\ UNCHANGED <<startLock, server, current, supervisor>>

BeginGrace ==
  /\ supervisor = "wait"
  /\ holders = {}
  /\ supervisor' = "grace"
  /\ UNCHANGED <<startLock, holders, server, current>>

CancelGrace ==
  /\ supervisor = "grace"
  /\ holders # {}
  /\ supervisor' = "wait"
  /\ UNCHANGED <<startLock, holders, server, current>>

Stop ==
  /\ supervisor = "grace"
  /\ holders = {}
  /\ startLock = "none"
  /\ server' = "down"
  /\ current' = "none"
  /\ supervisor' = "absent"
  /\ UNCHANGED <<startLock, holders>>

Crash ==
  /\ supervisor \in {"wait", "grace"}
  /\ supervisor' = "dead"
  /\ UNCHANGED <<startLock, holders, server, current>>

Watch ==
  /\ supervisor = "dead"
  /\ server' = "down"
  /\ current' = "none"
  /\ supervisor' = "absent"
  /\ UNCHANGED <<startLock, holders>>

Next ==
  \/ \E t \in Tests : LockStart(t) \/ Join(t) \/ Create(t) \/ Release(t)
  \/ BeginGrace
  \/ CancelGrace
  \/ Stop
  \/ Crash
  \/ Watch

Spec == Init /\ [][Next]_<<startLock, holders, server, current, supervisor>>

\* current names a run only while mysqld is up, or the supervisor has crashed
\* and the watch process has not stopped mysqld yet.
CurrentMatchesServer ==
  current = "run" <=> server = "up" \/ supervisor = "dead"

\* A holder sees a down server only after the watch process has cleared current.
HoldersDownMeansStopped ==
  holders # {} /\ server = "down" => current = "none" /\ supervisor = "absent"

====
