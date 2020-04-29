## `dogstatsd_buffer_size`

The `dogstatsd_buffer_size` parameter configures two things in Dogstatsd:

* How many bytes must be read each time the socket is read
* How many bytes maximum the PacketAssembler should put into one packet

If you have reports of malformed or incomplete packets received by the Dogstatsd server, it could mean
that the clients that are sending packets are larger than the size of this buffer. If the maximum size of the
packets sent by the clients can't be changed, consider increasing the size of `dogstatsd_buffer_size`
as a fallback.

The default value of this field is `8192`.

## `dogstatsd_packet_buffer_size` and `dogstatsd_packet_buffer_flush_timeout`

**Note: This is an internal configuration field subject to change (or to removal) without any notice.**

In order to batch the parsing of the metrics instead of parsing them one after the other every time
a metric is received, Dogstatsd is using a PacketsBuffer to group packets together.

This configuration field represents how many packets the packets buffer is batching. Decreasing the
size of this buffer will result in the packets buffer flushing more often the packets to the parser.
It will be most costly in CPU but could help stress the pipeline and have fewer packet drops when packet
drops are an issue.

The default value of `dogstatsd_packet_buffer_size` is `32`.

In the same manner, `dogstatsd_packet_buffer_flush_timeout` is a timer forcing a flush on the packets
buffer to the parser. Each time this timer is triggered, a flush is executed.

The default value of this timer is `100ms`, decreasing it to something small such as `20ms` or even `1ms`
will use more CPU, but will again stress the pipeline and should lead to fewer drops when packet drops
are an issue.

## dogstatsd_queue_size

**Note: This is an internal configuration field subject to change (or to removal) without any notice.**

This parameter represents how many packet sets flushed from the packets buffer to the parser could be
buffered. The idea is to read as fast as possible on the socket and to store packets here if the rest
of the pipeline is having slow-down for any reasons.

This is where most of the memory usage of Dogstatsd resides. It means that if you decrease the size of
this queue, the maximum memory usage of Dogstatsd should decrease, however, the amount of drops can
increase.

The default value of this field is `1024`.

## dogstatsd_string_interner_size

**Note: This is an internal configuration field subject to change (or to removal) without any notice.**

Dogstatsd relies on the Go garbage-collector for its memory management.
Garbage collection is not the most optimal solution in every cases, for instance
while manipulating a large amounts of different string values.

Dogstatsd, with tags and metrics names, manipulates a lot of different string values.
The string interner is responsible for caching some strings
as long as it can so that the garbage collector is not collecting them too often. The string interner resets when it is full.


Its default configuration is to cache 4096 strings. If you consider a default metric name size
of 100, it means that it could use up to 409Kb of heap memory.

Increasing its value slightly increases the memory usage but could help reduce the amount of
GC cycles, thus CPU usage.