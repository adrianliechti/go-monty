package monty

import (
	"time"

	"github.com/adrianliechti/go-monty/internal/pb"
)

// ClockMode selects what date.today(), datetime.now() and time.time() read.
type ClockMode int

const (
	// ClockSystem reads the real clock.
	ClockSystem ClockMode = iota
	// ClockHost asks RunOptions.OS (date_today, datetime_now, time).
	ClockHost
	// ClockFixed always reads OSPolicy.FixedTime, for reproducible runs.
	ClockFixed
)

// SleepMode selects what time.sleep() and asyncio.sleep() do.
type SleepMode int

const (
	// SleepSystem waits on the host, at most OSPolicy.MaxSleep per call and
	// Limits.MaxSleep in total.
	SleepSystem SleepMode = iota
	// SleepHost asks RunOptions.OS (sleep, async_sleep).
	SleepHost
	// SleepNone returns at once.
	SleepNone
)

// RandomMode selects where an unseeded random generator starts.
type RandomMode int

const (
	// RandomSystem seeds from OS entropy.
	RandomSystem RandomMode = iota
	// RandomHost asks RunOptions.OS for the seed bytes (urandom).
	RandomHost
	// RandomSeeded seeds as random.seed(OSPolicy.Seed) would.
	RandomSeeded
)

// defaultMaxSleep is upstream's cap on one system sleep.
const defaultMaxSleep = 10 * time.Second

// OSPolicy says where a session's clock, timezone, sleeps and randomness
// come from. The zero value is the real clock in UTC, OS entropy, and
// sleeps of up to ten seconds each.
type OSPolicy struct {
	Clock     ClockMode
	FixedTime time.Time
	// TimeZone is the zone naive datetimes read in; nil means UTC. A zone
	// from time.LoadLocation keeps its rules; any other is sent as its
	// current fixed offset.
	TimeZone *time.Location
	Sleep    SleepMode
	// MaxSleep caps one SleepSystem call; longer sleeps are cut short. Zero
	// means ten seconds.
	MaxSleep time.Duration
	Random   RandomMode
	Seed     int64
	// ProcessTime makes time.process_time() report the session's execution
	// time instead of zero.
	ProcessTime bool
}

func (p OSPolicy) maxSleep() time.Duration {
	if p.Sleep == SleepSystem && p.MaxSleep > 0 {
		return p.MaxSleep
	}
	return defaultMaxSleep
}

func (p OSPolicy) proto() *pb.OsPolicy {
	out := &pb.OsPolicy{}
	switch p.Clock {
	case ClockHost:
		out.Datetime = &pb.OsPolicy_CallHost{CallHost: unit()}
	case ClockFixed:
		out.Datetime = &pb.OsPolicy_Fixed{Fixed: &pb.FixedDateTime{
			UnixSeconds: p.FixedTime.Unix(),
			Microsecond: uint32(p.FixedTime.Nanosecond() / 1000),
		}}
	}
	if loc := p.TimeZone; loc != nil && loc != time.UTC {
		switch name := loc.String(); name {
		case "", "Local":
			out.Timezone = &pb.SandboxTimeZone{Zone: &pb.SandboxTimeZone_Fixed{Fixed: timezoneProto(loc)}}
		default:
			out.Timezone = &pb.SandboxTimeZone{Zone: &pb.SandboxTimeZone_Named{Named: name}}
		}
	}
	switch p.Sleep {
	case SleepSystem:
		if p.MaxSleep > 0 {
			micros := uint64(p.MaxSleep / time.Microsecond)
			out.Sleep = &pb.SleepMode{Mode: &pb.SleepMode_System{System: &pb.SystemSleep{MaxMicros: &micros}}}
		}
	case SleepHost:
		out.Sleep = &pb.SleepMode{Mode: &pb.SleepMode_CallHost{CallHost: unit()}}
	case SleepNone:
		out.Sleep = &pb.SleepMode{Mode: &pb.SleepMode_Zero{Zero: unit()}}
	}
	switch p.Random {
	case RandomHost:
		out.RandomStart = &pb.OsPolicy_RandomCallHost{RandomCallHost: unit()}
	case RandomSeeded:
		// Little-endian two's complement, as upstream's BigInt::to_signed_bytes_le.
		seed := make([]byte, 8)
		for i := range seed {
			seed[i] = byte(p.Seed >> (8 * i))
		}
		out.RandomStart = &pb.OsPolicy_Seed{Seed: &pb.RandomSeed{Value: &pb.RandomSeed_Int{Int: seed}}}
	}
	if p.ProcessTime {
		out.ProcessTime = &pb.OsPolicy_Elapsed{Elapsed: unit()}
	}
	return out
}
