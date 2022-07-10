<?php
declare(strict_types=1);

namespace App\Form\Data;

use Symfony\Component\Validator\Constraints as Assert;

class PasswordResetData
{
    #[Assert\NotBlank]
    #[Assert\Length(min: 8, max: 4096)]
    #[Assert\NotCompromisedPassword(skipOnError: true)]
    public string $password;
}
