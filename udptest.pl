#!/usr/bin/perl -w
use IO::Socket::INET;

my $s = IO::Socket::INET->new(
Proto => 'udp',
PeerHost => 'localhost',
PeerPort => 9125
) or die $!;

my $count = 0;
for (1..100000000) {
     $count++;
     my $msg = sprintf("a.b.c.d.e_com.mailchimp.f:1|c");
     defined(send($s, $msg, 0)) or print "send(): $!\n";
}