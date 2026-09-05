//! Core wasm module for go-monty.
//!
//! The Go host talks the same length-prefixed protobuf protocol as
//! `monty subprocess` (see the `monty-proto` crate). Each request frame is
//! copied into linear memory and handed to [`monty_dispatch`]; every event the
//! turn produces is pushed back to the host *as it happens* through the
//! imported `monty.event` function, so `print()` output streams instead of
//! waiting for the turn to end.
//!
//! Exports (all `extern "C"`, plain core wasm, no component model):
//!
//! * `monty_alloc(len) -> ptr`        allocate `len` bytes for a request frame
//! * `monty_free(ptr, len)`           free a buffer never passed to dispatch
//! * `monty_dispatch(ptr, len) -> outcome`
//!       consume the frame at `ptr` (freeing it) and run one turn, emitting
//!       each framed event via `monty.event`; returns 0 = continue,
//!       1 = shutdown, 2 = fatal
//! * `monty_protocol_version()`       the `monty-proto` protocol version
//! * `monty_version_ptr() / monty_version_len()`  the upstream monty version
//!
//! Imports:
//!
//! * `monty.event(ptr, len)`          one framed `ChildEvent` (4-byte LE
//!                                    length prefix + protobuf)
#![allow(unsafe_code)]

use std::cell::RefCell;

use monty_proto::worker::{Child, EventSink, HandleOutcome, fatal_error_event, protocol_violation};
use monty_proto::{FrameError, FrameReader, PROTOCOL_VERSION, pb, write_frame};

/// Counts allocations against the session's `max_memory` limit. Crossing the
/// hard ceiling aborts, which in wasm is a trap the host observes as an error
/// from `monty_dispatch`; the host then discards the instance.
#[global_allocator]
static ALLOC: monty_alloc::LimitedAllocator = monty_alloc::LimitedAllocator;

#[link(wasm_import_module = "monty")]
unsafe extern "C" {
    /// Delivers one framed event to the host.
    fn event(ptr: *const u8, len: u32);
}

thread_local! {
    /// The session worker, retained for the lifetime of this instance.
    static CHILD: RefCell<Child> = RefCell::new(Child::default());
}

const OUTCOME_CONTINUE: u32 = 0;
const OUTCOME_SHUTDOWN: u32 = 1;
const OUTCOME_FATAL: u32 = 2;

/// Frames each event and hands it to the host immediately.
struct HostSink {
    buf: Vec<u8>,
}

impl EventSink for HostSink {
    fn send(&mut self, event: &pb::ChildEvent) -> Result<(), FrameError> {
        self.buf.clear();
        // `write_frame` rejects oversize frames before writing anything.
        write_frame(&mut self.buf, event)?;
        // SAFETY: the host reads exactly `len` bytes from `ptr` during the
        // call and keeps its own copy.
        unsafe { self::event(self.buf.as_ptr(), self.buf.len() as u32) };
        Ok(())
    }
}

/// Allocates `len` bytes the host fills with one request frame.
#[unsafe(no_mangle)]
pub extern "C" fn monty_alloc(len: u32) -> *mut u8 {
    let mut buf: Vec<u8> = Vec::with_capacity(len as usize);
    let ptr = buf.as_mut_ptr();
    std::mem::forget(buf);
    ptr
}

/// Frees a buffer from `monty_alloc` that was never passed to `monty_dispatch`.
#[unsafe(no_mangle)]
pub extern "C" fn monty_free(ptr: *mut u8, len: u32) {
    if !ptr.is_null() {
        // SAFETY: `ptr`/`len` came from `monty_alloc` with this capacity.
        drop(unsafe { Vec::from_raw_parts(ptr, 0, len as usize) });
    }
}

/// Runs one protocol turn over the framed request at `ptr`, taking ownership
/// of the buffer, and streams the turn's events to the host.
#[unsafe(no_mangle)]
pub extern "C" fn monty_dispatch(ptr: *mut u8, len: u32) -> u32 {
    // SAFETY: `ptr`/`len` came from `monty_alloc(len)` and the host filled all
    // `len` bytes before calling.
    let request = unsafe { Vec::from_raw_parts(ptr, len as usize, len as usize) };
    let mut sink = HostSink { buf: Vec::new() };

    let (outcome, limit) = CHILD.with_borrow_mut(|child| {
        let outcome = dispatch(child, &request, &mut sink);
        let budget = child.session_budget();
        let limit = monty_alloc::set_limit(budget.max_memory, budget.type_check);
        (outcome, limit)
    });
    drop(request);

    let mut code = match outcome {
        HandleOutcome::Continue => OUTCOME_CONTINUE,
        HandleOutcome::Shutdown => OUTCOME_SHUTDOWN,
        HandleOutcome::Fatal => OUTCOME_FATAL,
    };
    if let Err(error) = limit {
        let _ = sink.send(&fatal_error_event(error));
        code = OUTCOME_FATAL;
    }
    code
}

/// Decodes and handles one request frame; mirrors `monty-proto`'s buffered
/// `dispatch_frame` but with a streaming sink.
fn dispatch(child: &mut Child, request_frame: &[u8], sink: &mut HostSink) -> HandleOutcome {
    let mut reader = FrameReader::new(request_frame);
    match reader.read::<pb::ParentRequest>() {
        Ok(Some(request)) => match child.handle(request, sink) {
            Ok(outcome) => outcome,
            // an oversize suspension (or any unrecoverable error) leaves no
            // resume point, so emit a fatal last gasp and stop the worker
            Err(FrameError::FrameTooLarge { len, max }) => {
                let _ =
                    sink.send(&child.fatal_event(&format!("response frame of {len} bytes exceeds maximum of {max} bytes")));
                HandleOutcome::Shutdown
            }
            Err(_) => HandleOutcome::Shutdown,
        },
        Ok(None) => HandleOutcome::Continue,
        Err(FrameError::Decode(err)) => {
            let _ = sink.send(&protocol_violation(&format!("malformed request: {err}")));
            HandleOutcome::Continue
        }
        Err(err) => {
            let _ = sink.send(&child.fatal_event(&format!("malformed request frame: {err}")));
            HandleOutcome::Shutdown
        }
    }
}

/// The wire protocol version this module serves.
#[unsafe(no_mangle)]
pub extern "C" fn monty_protocol_version() -> u32 {
    PROTOCOL_VERSION
}

#[unsafe(no_mangle)]
pub extern "C" fn monty_version_ptr() -> *const u8 {
    monty_types::MONTY_VERSION.as_ptr()
}

#[unsafe(no_mangle)]
pub extern "C" fn monty_version_len() -> u32 {
    u32::try_from(monty_types::MONTY_VERSION.len()).unwrap_or(0)
}
