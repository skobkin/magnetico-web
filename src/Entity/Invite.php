<?php

namespace App\Entity;

use Doctrine\ORM\Mapping as ORM;

/**
 * @ORM\Table(name="invites", schema="users")
 * @ORM\Entity()
 */
class Invite
{
    /**
     * @var int
     *
     * @ORM\Id()
     * @ORM\GeneratedValue(strategy="AUTO")
     * @ORM\Column(name="id", type="integer")
     */
    private $id;

    /**
     * @var User
     *
     * @ORM\ManyToOne(targetEntity="App\Entity\User")
     * @ORM\JoinColumn(name="user_id", nullable=false)
     */
    private $user;

    /**
     * @var string
     *
     * @ORM\Column(name="code", type="string", length=32)
     */
    private $code;

    /**
     * @var User|null
     *
     * @ORM\ManyToOne(targetEntity="App\Entity\User")
     * @ORM\JoinColumn(name="used_by_id", nullable=true)
     */
    private $usedBy;

    public function __construct(User $user)
    {
        $this->user = $user;
        $this->code = md5(random_bytes(100));
    }

    public function getId(): int
    {
        return $this->id;
    }

    public function getUser(): User
    {
        return $this->user;
    }

    public function getCode(): string
    {
        return $this->code;
    }

    public function getUsedBy(): ?User
    {
        return $this->usedBy;
    }
}