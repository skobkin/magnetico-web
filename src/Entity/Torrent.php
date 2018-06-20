<?php

namespace App\Entity;

use Doctrine\Common\Collections\ArrayCollection;
use Doctrine\ORM\Mapping as ORM;

/**
 * @ORM\Table(name="torrents", indexes={
 *     @ORM\Index(name="discovered_on_index", columns={"discovered_on"}),
 *     @ORM\Index(name="info_hash_index", columns={"info_hash"})
 * })
 * @ORM\Entity(readOnly=true, repositoryClass="App\Repository\TorrentRepository")
 */
class Torrent
{
    /**
     * @var int
     *
     * @ORM\Column(name="id", type="integer")
     * @ORM\Id
     */
    private $id;

    /**
     * @var string
     *
     * @ORM\Column(name="info_hash", type="blob", nullable=false)
     */
    private $infoHash;

    /**
     * @var string
     *
     * @ORM\Column(name="name", type="text", nullable=false)
     */
    private $name;

    /**
     * @var int
     *
     * @ORM\Column(name="total_size", type="integer", nullable=false)
     */
    private $totalSize;

    /**
     * @var int
     *
     * @ORM\Column(name="discovered_on", type="integer", nullable=false)
     */
    private $discoveredOn;

    /**
     * @var File[]|ArrayCollection
     *
     * @ORM\OneToMany(targetEntity="App\Entity\File", fetch="EXTRA_LAZY", mappedBy="torrent")
     */
    private $files;

    public function getId(): int
    {
        return $this->id;
    }

    public function getInfoHash()
    {
        return $this->infoHash;
    }

    public function getInfoHashAsHex(): string
    {
        return bin2hex(stream_get_contents($this->infoHash));
    }

    public function getName(): string
    {
        return $this->name;
    }

    public function getTotalSize(): int
    {
        return $this->totalSize;
    }

    public function getDiscoveredOn(): int
    {
        return $this->discoveredOn;
    }

    /**
     * @return File[]|ArrayCollection
     */
    public function getFiles(): iterable
    {
        return $this->files;
    }
}
