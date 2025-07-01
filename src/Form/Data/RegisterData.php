<?php
declare(strict_types=1);

namespace App\Form\Data;

use Symfony\Component\Validator\Constraints as Assert;
use App\Validator\Constraints as AppAssert;

/**
 * @todo implement UniqueEntity constraint for DTO and use it here
 */
class RegisterData
{
    #[Assert\NotBlank]
    #[Assert\Length(min: 2, max: 25)]
    public string $username;

    #[Assert\NotBlank]
    #[Assert\Length(min: 8, max: 4096)]
    public string $password;

    #[Assert\Email]
    public string $email;

    #[Assert\NotBlank]
    #[Assert\Length(min: 32, max: 32)]
    #[AppAssert\ValidInvite()]
    public string $inviteCode;

    public function __construct(?string $inviteCode = null)
    {
        $this->inviteCode = $inviteCode;
    }
}
