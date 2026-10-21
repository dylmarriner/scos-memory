use parking_lot::Mutex;
use scos_common::{MemorySyncEvent, Result};

/// Sync queue for memory synchronization across nodes (Phase 6).
pub struct SyncQueue {
    pending: Mutex<Vec<MemorySyncEvent>>,
}

impl SyncQueue {
    pub fn new() -> Self {
        Self {
            pending: Mutex::new(Vec::new()),
        }
    }

    /// Add a sync event to the queue.
    pub fn push(&self, event: MemorySyncEvent) -> Result<()> {
        let mut pending = self.pending.lock();
        pending.push(event);
        Ok(())
    }

    /// Get all pending sync events.
    pub fn drain(&self) -> Vec<MemorySyncEvent> {
        let mut pending = self.pending.lock();
        std::mem::take(&mut *pending)
    }

    /// Get count of pending events.
    pub fn len(&self) -> usize {
        let pending = self.pending.lock();
        pending.len()
    }
}
